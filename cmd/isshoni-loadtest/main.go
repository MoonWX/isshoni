// Command isshoni-loadtest is the load test of an isshoni server (docs/m1/02-sfu.md §15.2): simulated friends who
// share and watch through a real server, with the server's own numbers and what every viewer received.
//
//	isshoni-loadtest -server https://example.org -admin admin   # password from ISSHONI_LOADTEST_PASSWORD or a prompt
//	  [-scenario focus|all-high|smoke]  [-publishers 10] [-subscribers 10] [-duration 10m] [-warmup 60s]
//	  [-full 1920x1080@60:8M] [-preview 640x360@15:300k] [-gop 3s] [-profile 6400|42e0] [-decodable]
//	  [-focus-rotate 30s] [-login-interval 3s] [-json out.json] [-keep-users]
//
// Setup (auth.go): the tool logs in as the admin, creates one invite with publishers + subscribers uses, and
// registers the accounts lt-<run>-<n> with random passwords, one request per -login-interval (03's auth-ip bucket).
// Each simulated client is in this process (client.go): 01's Go signaling client on /ws with the account's session
// cookie (kind tool), 02's publish.Publisher on internal/media/fake (Synthetic, or Decodable with -decodable) for a
// publisher, and sfutest.Viewer for a subscriber, all joined to the default room. Teardown deletes the accounts
// (DELETE /api/v1/admin/users/{id}) unless -keep-users, revokes the invite and logs the admin out.
//
// Scenarios (scenario.go): focus, the plan's pass test (every subscriber watches one share at the full layer with
// its sound and the others as thumbnails, and moves its focus every -focus-rotate); all-high, the stress test that
// raises 10 thumbnails to the full layer every 30 s until the loss or the server's CPU is too high (report only);
// smoke, the CI run (2×2, 30 s, -decodable, the focus checks with relaxed limits). Explicit flags override a
// scenario's defaults. With -subscribers 0 the tool only publishes for -duration and skips the viewers' checks, so
// a browser can be the viewer (web/e2e/decodable.spec.ts).
//
// What is measured (measure.go) after the warm-up: the server's CPU, memory per DownTrack and media egress from
// GET /api/v1/admin/dashboard every 5 s; the loss before and after NACK/RTX on every subscriber track; the keyframe
// requests that reach each publisher layer; the time from a subscribe.update to the first packet of the new layer;
// and the offset of the fake media's flash frames from their beeps at the viewers. The result (report.go) is a
// human summary on stdout and, with -json, the JSON report in a file ("-" puts it on stdout and the summary on
// stderr). Progress goes to stderr.
//
// Exit codes: 0 the run passed; 1 it failed (a check, a client that lost its connection, media that never came, a
// teardown that left accounts behind); 2 a setup error (a usage error, a server that can't be reached, a refused
// login, an account or a share the server would not create); 130 interrupted (SIGINT or SIGTERM: the accounts are
// deleted all the same).
//
// This is test tooling: the release builds only cmd/isshoni (06), so no release artifact contains it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/media/fake"
)

// Exit codes (02 §15.2).
const (
	exitPass        = 0
	exitFail        = 1
	exitSetup       = 2
	exitInterrupted = 130
)

// passwordEnv holds the admin's password (04 §4.2 reserves the name for this tool).
const passwordEnv = "ISSHONI_LOADTEST_PASSWORD"

// Limits of the command line.
const (
	// maxAccounts is what one invite allows (03 §12.4.5: maxUses 1–1000).
	maxAccounts = 1000
	// maxPublishers keeps a subscriber's subscriptions in one subscribe.update (01 §8.9: at most 64 items; a
	// subscriber watches every share but its own).
	maxPublishers = 64
)

// env is what run takes from the process, so that a test can run the tool in its own process.
type env struct {
	stdout, stderr io.Writer
	getenv         func(string) string
	// readPassword prompts on the terminal and reads a line without echo; nil when stdin is no terminal.
	readPassword func(prompt string) (string, error)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], processEnv())
	stop()
	os.Exit(code)
}

// processEnv is the env of the real process.
func processEnv() env {
	e := env{stdout: os.Stdout, stderr: os.Stderr, getenv: os.Getenv}
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) { //nolint:gosec // G115: a file descriptor fits an int
		e.readPassword = func(prompt string) (string, error) {
			fmt.Fprint(os.Stderr, prompt)
			b, err := term.ReadPassword(fd)
			fmt.Fprintln(os.Stderr)
			return string(b), err
		}
	}
	return e
}

// run is the whole command: it returns the exit code.
func run(ctx context.Context, args []string, e env) int {
	cfg, err := parseFlags(args, e.stderr)
	if errors.Is(err, flag.ErrHelp) {
		return exitPass
	}
	if err != nil {
		fmt.Fprintf(e.stderr, "isshoni-loadtest: %v\n", err)
		return exitSetup
	}
	password := e.getenv(passwordEnv)
	if password == "" {
		if e.readPassword == nil {
			fmt.Fprintf(e.stderr, "isshoni-loadtest: no password for %s: set %s or run on a terminal\n", cfg.admin, passwordEnv)
			return exitSetup
		}
		if password, err = e.readPassword("Password for " + cfg.admin + ": "); err != nil || password == "" {
			fmt.Fprintf(e.stderr, "isshoni-loadtest: no password for %s\n", cfg.admin)
			return exitSetup
		}
	}
	cfg.password = logx.Secret(password)

	level := new(slog.LevelVar)
	if cfg.verbose {
		level.Set(slog.LevelDebug)
	}
	log := logx.New(logx.Options{Level: level, Out: e.stderr})
	rep, err := newLoadTest(cfg, log).run(ctx)

	summary := e.stdout
	if cfg.jsonPath == "-" {
		summary = e.stderr
	}
	if rep != nil {
		writeSummary(summary, rep)
		if cfg.jsonPath != "" {
			if werr := writeJSON(cfg.jsonPath, e.stdout, rep); werr != nil {
				fmt.Fprintf(e.stderr, "isshoni-loadtest: the JSON report: %v\n", werr)
				if err == nil && rep.Pass {
					return exitFail
				}
			}
		}
	}
	var se *setupError
	switch {
	case errors.As(err, &se):
		fmt.Fprintf(e.stderr, "isshoni-loadtest: setup: %v\n", se.err)
		return exitSetup
	case errors.Is(err, errInterrupted):
		fmt.Fprintln(e.stderr, "isshoni-loadtest: interrupted")
		return exitInterrupted
	case err != nil:
		fmt.Fprintf(e.stderr, "isshoni-loadtest: %v\n", err)
		return exitFail
	case rep == nil || !rep.Pass:
		return exitFail
	}
	return exitPass
}

// setupError is an error before the measured run could begin: exit 2.
type setupError struct{ err error }

func (e *setupError) Error() string { return "setup: " + e.err.Error() }
func (e *setupError) Unwrap() error { return e.err }

// setupErrorf returns a *setupError.
func setupErrorf(format string, args ...any) error {
	return &setupError{err: fmt.Errorf(format, args...)}
}

// errInterrupted ends a run that SIGINT or SIGTERM stopped: exit 130.
var errInterrupted = errors.New("interrupted")

// config is everything a run needs: the command line, resolved against the scenario's defaults.
type config struct {
	server   *url.URL // the server's origin: scheme and host
	admin    string
	password logx.Secret

	scenario      scenario
	publishers    int
	subscribers   int
	duration      time.Duration // from the moment every client is up, warm-up included
	warmup        time.Duration
	layers        []fake.VideoLayer // f, then q
	gop           time.Duration
	profile       string // the H.264 profile key of the fake video
	decodable     bool
	focusRotate   time.Duration
	loginInterval time.Duration
	jsonPath      string // "" none, "-" stdout
	keepUsers     bool
	verbose       bool

	// Fixed by the spec; tests shorten them.
	dashboardEvery time.Duration // the server's numbers (02 §15.2: every 5 s)
	statsEvery     time.Duration // the publishers' counters, for the PLI windows
	stepEvery      time.Duration // all-high: one step every 30 s
	stepSize       int           // all-high: 10 thumbnails become full layers per step
	setupTimeout   time.Duration // shares live, subscriptions delivering
}

// Defaults of the flags that no scenario sets.
const (
	defaultGOP            = 3 * time.Second
	defaultFocusRotate    = 30 * time.Second
	defaultLoginInterval  = 3 * time.Second
	defaultDashboardEvery = 5 * time.Second
	defaultStatsEvery     = time.Second
	defaultStepEvery      = 30 * time.Second
	defaultStepSize       = 10
	defaultSetupTimeout   = 60 * time.Second
	defaultFull           = "1920x1080@60:8M"
	defaultPreview        = "640x360@15:300k"
)

// parseFlags parses the command line. A usage problem is an error; -h prints the usage and returns flag.ErrHelp.
func parseFlags(args []string, stderr io.Writer) (*config, error) {
	fs := flag.NewFlagSet("isshoni-loadtest", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		server      = fs.String("server", "", "the server's `URL`, e.g. https://watch.example.com (required)")
		admin       = fs.String("admin", "", "an admin's `username` (required); the password comes from "+passwordEnv+" or a prompt")
		scen        = fs.String("scenario", "focus", "`focus`, all-high or smoke")
		publishers  = fs.Int("publishers", 0, "how many share (default: the scenario's)")
		subscribers = fs.Int("subscribers", 0, "how many watch (default: the scenario's); 0 only publishes")
		duration    = fs.Duration("duration", 0, "the run from the moment every client is up, warm-up included (default: the scenario's)")
		warmup      = fs.Duration("warmup", 0, "what the checks leave out at the start (default: the scenario's)")
		full        = fs.String("full", defaultFull, "the full layer f: `WxH@FPS:BITRATE` (-decodable: 640x360@30, the bitrate is ignored)")
		preview     = fs.String("preview", defaultPreview, "the preview layer q: `WxH@FPS:BITRATE` (-decodable: 320x180@15)")
		gop         = fs.Duration("gop", defaultGOP, "the keyframe interval of the fake video")
		profile     = fs.String("profile", fake.DefaultProfile, "the H.264 profile of the fake video: 6400 or 42e0 (-decodable: 42e0)")
		decodable   = fs.Bool("decodable", false, "send video that browsers decode (default: the scenario's)")
		rotate      = fs.Duration("focus-rotate", defaultFocusRotate, "how often a subscriber moves its focus to the next share")
		interval    = fs.Duration("login-interval", defaultLoginInterval, "the time between two sign-ups (03's auth-ip bucket)")
		jsonPath    = fs.String("json", "", "write the JSON report to this `file` (\"-\": stdout)")
		keepUsers   = fs.Bool("keep-users", false, "keep the accounts after the run")
		verbose     = fs.Bool("v", false, "log at debug level")
	)
	fs.Usage = func() { usage(fs) }
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	sc, ok := scenarios[*scen]
	if !ok {
		return nil, fmt.Errorf("-scenario %q: want focus, all-high or smoke", *scen)
	}
	cfg := &config{
		admin: strings.TrimSpace(*admin), scenario: sc,
		publishers: sc.publishers, subscribers: sc.subscribers, duration: sc.duration, warmup: sc.warmup,
		gop: *gop, profile: *profile, decodable: sc.decodable, focusRotate: *rotate, loginInterval: *interval,
		jsonPath: *jsonPath, keepUsers: *keepUsers, verbose: *verbose,
		dashboardEvery: defaultDashboardEvery, statsEvery: defaultStatsEvery, stepEvery: defaultStepEvery,
		stepSize: defaultStepSize, setupTimeout: defaultSetupTimeout,
	}
	if set["publishers"] {
		cfg.publishers = *publishers
	}
	if set["subscribers"] {
		cfg.subscribers = *subscribers
	}
	if set["duration"] {
		cfg.duration = *duration
	}
	if set["warmup"] {
		cfg.warmup = *warmup
	}
	if set["decodable"] {
		cfg.decodable = *decodable
	}

	var err error
	if cfg.server, err = parseServer(*server); err != nil {
		return nil, err
	}
	switch {
	case cfg.admin == "":
		return nil, errors.New("-admin is required")
	case cfg.publishers < 1 || cfg.publishers > maxPublishers:
		return nil, fmt.Errorf("-publishers %d: want 1 to %d", cfg.publishers, maxPublishers)
	case cfg.subscribers < 0:
		return nil, fmt.Errorf("-subscribers %d: want 0 or more", cfg.subscribers)
	case cfg.subscribers == 0 && sc.layout == layoutAllHigh:
		return nil, errors.New("-scenario all-high needs subscribers")
	case cfg.publishers+cfg.subscribers > maxAccounts:
		return nil, fmt.Errorf("-publishers + -subscribers = %d: one invite allows at most %d accounts",
			cfg.publishers+cfg.subscribers, maxAccounts)
	case cfg.duration <= 0:
		return nil, fmt.Errorf("-duration %v: want more than 0", cfg.duration)
	case cfg.warmup < 0 || cfg.warmup >= cfg.duration:
		return nil, fmt.Errorf("-warmup %v: want at least 0 and less than -duration %v", cfg.warmup, cfg.duration)
	case cfg.gop <= 0:
		return nil, fmt.Errorf("-gop %v: want more than 0", cfg.gop)
	case cfg.focusRotate <= 0:
		return nil, fmt.Errorf("-focus-rotate %v: want more than 0", cfg.focusRotate)
	case cfg.loginInterval < 0:
		return nil, fmt.Errorf("-login-interval %v: want 0 or more", cfg.loginInterval)
	}

	if cfg.decodable {
		if set["profile"] && cfg.profile != fake.DecodableProfile {
			return nil, fmt.Errorf("-profile %s: -decodable video is always %s", cfg.profile, fake.DecodableProfile)
		}
		cfg.profile = fake.DecodableProfile
	} else if cfg.profile != fake.DefaultProfile && cfg.profile != fake.DecodableProfile {
		return nil, fmt.Errorf("-profile %q: want %s or %s", cfg.profile, fake.DefaultProfile, fake.DecodableProfile)
	}
	if cfg.layers, err = parseLayers(*full, *preview, set["full"], set["preview"], cfg.decodable); err != nil {
		return nil, err
	}
	return cfg, nil
}

// usage prints the synopsis, the flags and the exit codes.
func usage(fs *flag.FlagSet) {
	w := fs.Output()
	fmt.Fprint(w, `Usage: isshoni-loadtest -server URL -admin NAME [flags]

Simulated friends share and watch through an isshoni server; the run passes or fails the checks of the scenario
(docs/m1/02-sfu.md §15.2). The admin's password comes from `+passwordEnv+`, or a prompt on a terminal.

Flags:
`)
	fs.PrintDefaults()
	fmt.Fprint(w, `
Exit codes: 0 pass, 1 fail, 2 setup error (usage, server unreachable, login or accounts refused), 130 interrupted.
`)
}

// parseServer checks the -server URL: http or https, a host, nothing after it but "/".
func parseServer(s string) (*url.URL, error) {
	if s == "" {
		return nil, errors.New("-server is required")
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("-server %q: %w", s, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") ||
		u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("-server %q: want http(s)://host[:port], the server's address", s)
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host}, nil
}

// parseLayers resolves the -full and -preview flags into the fake video layers f and q. Decodable video takes its
// size and frame rate from them only when they were given (its default is fake.DefaultDecodableLayers) and never
// its bitrate, which follows from the picture.
func parseLayers(full, preview string, fullSet, previewSet, decodable bool) ([]fake.VideoLayer, error) {
	layers := []fake.VideoLayer{}
	defaults := fake.DefaultDecodableLayers()
	for i, l := range []struct {
		rid, flag, value string
		set              bool
	}{{"f", "full", full, fullSet}, {"q", "preview", preview, previewSet}} {
		if decodable && !l.set {
			layers = append(layers, defaults[i])
			continue
		}
		layer, err := parseLayer(l.rid, l.value)
		if err != nil {
			return nil, fmt.Errorf("-%s %q: %w", l.flag, l.value, err)
		}
		switch {
		case decodable:
			layer.Bitrate = defaults[i].Bitrate // an estimate, for the report; Decodable ignores it
		case layer.Bitrate <= 0:
			return nil, fmt.Errorf("-%s %q: synthetic video needs a bitrate, e.g. %s", l.flag, l.value, defaultFull)
		}
		layers = append(layers, layer)
	}
	return layers, nil
}

// parseLayer parses WxH@FPS[:BITRATE]: "1920x1080@60:8M", "640x360@15:300k". The bitrate is in bit/s, with an
// optional k (×1000) or M (×1,000,000) suffix; without one it is 0.
func parseLayer(rid, s string) (fake.VideoLayer, error) {
	l := fake.VideoLayer{RID: rid}
	size, rest, ok := strings.Cut(s, "@")
	if !ok {
		return l, errors.New("want WxH@FPS:BITRATE")
	}
	w, h, ok := strings.Cut(size, "x")
	if !ok {
		return l, errors.New("want WxH@FPS:BITRATE")
	}
	fps, rate, hasRate := strings.Cut(rest, ":")
	var err error
	if l.Width, err = strconv.Atoi(w); err != nil || l.Width <= 0 || l.Width%2 != 0 {
		return l, fmt.Errorf("width %q: want a positive even number", w)
	}
	if l.Height, err = strconv.Atoi(h); err != nil || l.Height <= 0 || l.Height%2 != 0 {
		return l, fmt.Errorf("height %q: want a positive even number", h)
	}
	if l.FPS, err = strconv.Atoi(fps); err != nil || l.FPS < 1 || l.FPS > 240 {
		return l, fmt.Errorf("frame rate %q: want 1 to 240", fps)
	}
	if hasRate {
		if l.Bitrate, err = parseBitrate(rate); err != nil {
			return l, err
		}
	}
	return l, nil
}

// parseBitrate parses "8M", "300k", "1.5M" or "250000" into bit/s.
func parseBitrate(s string) (int, error) {
	mult := 1.0
	num := s
	switch {
	case strings.HasSuffix(s, "M"):
		mult, num = 1e6, strings.TrimSuffix(s, "M")
	case strings.HasSuffix(s, "k"), strings.HasSuffix(s, "K"):
		mult, num = 1e3, s[:len(s)-1]
	}
	v, err := strconv.ParseFloat(num, 64)
	bps := math.Round(v * mult)
	if err != nil || !(bps >= 1000 && bps <= 1e9) { // NaN too
		return 0, fmt.Errorf("bitrate %q: want 1k to 1000M, e.g. 8M or 300k", s)
	}
	return int(bps), nil
}

// formatLayer is parseLayer's form of a layer, for the report.
func formatLayer(l fake.VideoLayer) string {
	return fmt.Sprintf("%dx%d@%d:%s", l.Width, l.Height, l.FPS, formatBitrate(l.Bitrate))
}

// formatBitrate writes bit/s as parseBitrate reads it.
func formatBitrate(bps int) string {
	switch {
	case bps >= 1e6 && bps%1e5 == 0:
		return strconv.FormatFloat(float64(bps)/1e6, 'f', -1, 64) + "M"
	case bps >= 1e3 && bps%100 == 0:
		return strconv.FormatFloat(float64(bps)/1e3, 'f', -1, 64) + "k"
	}
	return strconv.Itoa(bps)
}
