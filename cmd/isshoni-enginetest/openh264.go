package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/native"
)

// openh264.go is README WA51's command (04 §14: "openh264 install|verify|remove", the app's own Go code, hash checks):
// the binding's OpenH264 helpers, as 03 calls them when the user turns OpenH264 on or off, for the owner's PC and
// M-OPENH264 (04 §18.4).

func init() {
	register(command{name: "openh264", summary: "install, verify or remove Cisco's OpenH264 with the app's own helpers",
		run: cmdOpenH264})
}

// openH264Helpers are the binding's helpers (tests replace them).
type openH264Helpers struct {
	install func(ctx context.Context, c *http.Client, dir string) (string, error)
	verify  func(path string) error
	remove  func(dir string) error
}

var nativeOpenH264 = openH264Helpers{install: native.InstallOpenH264, verify: native.VerifyOpenH264,
	remove: native.RemoveOpenH264}

// openH264Result is what the command prints (JSON with -json).
type openH264Result struct {
	Action  string `json:"action"` // install, verify, remove
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	Path    string `json:"path,omitempty"` // the DLL (install, verify)
	Version string `json:"version"`        // the pin
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
	// verify -engine: what the engine says after im_set_openh264_path and a full probe.
	Engine *openH264EngineResult `json:"engine,omitempty"`
}

type openH264EngineResult struct {
	Loaded        bool     `json:"loaded"`        // im_build_info's openh264.loaded
	Version       string   `json:"version"`       // im_build_info's openh264.version
	Status        string   `json:"status"`        // the probe entry's status ("" when tested)
	Reason        string   `json:"reason"`        // its token
	Tested        bool     `json:"tested"`        // IM_ENCF_AVAILABLE
	LiveBitrate   bool     `json:"liveBitrate"`   // IM_ENCF_LIVE_BITRATE (the probe's live-bitrate check)
	Profiles      []string `json:"profiles"`      // "42e0" only
	SetPathResult string   `json:"setPathResult"` // "" or im_set_openh264_path's error
}

// cmdOpenH264: install downloads, bounds, hashes and renames the pinned release into -dir (native.InstallOpenH264);
// verify checks the installed file's size and SHA-256 (native.VerifyOpenH264), and with -engine also hands it to the
// engine (SetOpenH264Path: the version check) and runs a full probe, which must test-encode it; remove deletes it
// (native.RemoveOpenH264). Exit 0 pass, 1 a file that is not the pinned release or an engine that refuses it, 2 the
// environment (no network, no engine DLL, nothing installed), 64 usage.
func cmdOpenH264(ctx context.Context, g *globals, args []string) int {
	return runOpenH264(ctx, g, args, nativeOpenH264)
}

func runOpenH264(ctx context.Context, g *globals, args []string, h openH264Helpers) int {
	const synopsis = "install|verify|remove [-dir DIR] [-engine]"
	if len(args) == 0 || (args[0] != "install" && args[0] != "verify" && args[0] != "remove") {
		fmt.Fprintf(g.stderr, "usage: isshoni-enginetest openh264 %s\n", synopsis)
		if len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help") {
			return exitPass
		}
		return exitUsage
	}
	action := args[0]
	fs := newFlags(g, "openh264 "+action, synopsis)
	dir := fs.String("dir", defaultCodecsDir(),
		"the codecs `folder` (default: the app's, %LOCALAPPDATA%\\isshoni\\codecs on Windows)")
	withEngine := fs.Bool("engine", false, "verify: also load it into the engine (-dll) and run a full probe")
	if code, ok := parseFlags(fs, args[1:]); !ok {
		return code
	}
	if *dir == "" {
		fmt.Fprintln(g.stderr, "openh264: no -dir (and no user cache folder for the default)")
		return exitUsage
	}
	if *withEngine && action != "verify" {
		fmt.Fprintln(g.stderr, "openh264: -engine goes with verify")
		return exitUsage
	}
	pin := native.OpenH264
	r := openH264Result{Action: action, Version: pin.Version, SHA256: pin.SHA256, Size: pin.Size}
	code := exitPass
	var err error
	switch action {
	case "install":
		r.Path, err = h.install(ctx, nil, *dir)
	case "verify":
		r.Path = filepath.Join(*dir, pin.FileName)
		err = h.verify(r.Path)
		if err == nil && *withEngine {
			r.Engine, code = verifyInEngine(ctx, g, r.Path)
		}
	case "remove":
		err = h.remove(*dir)
	}
	if err != nil {
		r.Error = err.Error()
		code = openH264Code(err)
	}
	r.OK = code == exitPass
	printOpenH264(g.stdout, r, g.json)
	if err != nil {
		fmt.Fprintln(g.stderr, "openh264:", err)
	}
	return code
}

// openH264Code: bytes that are not the pinned release fail (1); a download that didn't happen, a missing file, a
// folder the tool can't write are the environment (2).
func openH264Code(err error) int {
	var me *media.Error
	if errors.As(err, &me) && me.Code == media.CodeDependency && me.Reason == "corrupt" {
		return exitFail
	}
	if errors.As(err, &me) && me.Code == media.CodeTimeout {
		return exitEnv
	}
	return envCode(err)
}

// verifyInEngine loads the engine, hands it the verified file and runs a full probe: the engine must report the
// pinned version loaded and OpenH264 tested (CB only). OpenH264 is turned off again before the engine closes.
func verifyInEngine(ctx context.Context, g *globals, path string) (*openH264EngineResult, int) {
	e, code, err := g.openEngine(true)
	if err != nil {
		fmt.Fprintln(g.stderr, "openh264: the engine didn't load:", err)
		return nil, code
	}
	defer func() { _ = e.Close() }()
	res := &openH264EngineResult{}
	if err := e.SetOpenH264Path(path); err != nil {
		res.SetPathResult = err.Error()
		fmt.Fprintln(g.stderr, "openh264: the engine refused the file:", err)
		return res, exitFail
	}
	defer func() { _ = e.SetOpenH264Path("") }()
	p, err := e.Probe(ctx, true)
	if err != nil {
		fmt.Fprintln(g.stderr, "openh264: probe:", err)
		return res, envCode(err)
	}
	b := e.BuildInfo()
	res.Loaded, res.Version = b.OpenH264.Loaded, b.OpenH264.Version
	found := false
	for _, enc := range p.Encoders {
		if enc.Name == "openh264" {
			found = true
			res.Status, res.Reason, res.Tested = string(enc.Status), enc.Reason, enc.Tested
			res.LiveBitrate, res.Profiles = enc.LiveBitrate, enc.Profiles
		}
	}
	switch {
	case !found:
		fmt.Fprintln(g.stderr, "FAIL: the probe lists no openh264 entry")
		return res, exitFail
	case !res.Loaded || res.Version != native.OpenH264.Version:
		fmt.Fprintf(g.stderr, "FAIL: build info says OpenH264 loaded=%v version %q, want %s\n", res.Loaded, res.Version,
			native.OpenH264.Version)
		return res, exitFail
	case !res.Tested:
		fmt.Fprintf(g.stderr, "FAIL: OpenH264's probe entry is %s (%s), not tested\n", res.Status, res.Reason)
		return res, exitFail
	case len(res.Profiles) != 1 || res.Profiles[0] != "42e0":
		fmt.Fprintf(g.stderr, "FAIL: OpenH264's profiles %v, want [42e0]\n", res.Profiles)
		return res, exitFail
	}
	return res, exitPass
}

func printOpenH264(w io.Writer, r openH264Result, asJSON bool) {
	if asJSON {
		data, err := json.MarshalIndent(r, "", "  ")
		if err == nil {
			_, _ = w.Write(append(data, '\n'))
		}
		return
	}
	state := "ok"
	if !r.OK {
		state = "FAILED"
	}
	fmt.Fprintf(w, "openh264 %s: %s (pin %s, %d bytes, sha256 %s)\n", r.Action, state, r.Version, r.Size, r.SHA256)
	if r.Path != "" && r.OK {
		fmt.Fprintf(w, "  file: %s\n", r.Path)
	}
	if e := r.Engine; e != nil {
		fmt.Fprintf(w, "  engine: loaded=%v version=%s tested=%v liveBitrate=%v profiles=%v status=%q reason=%q\n",
			e.Loaded, e.Version, e.Tested, e.LiveBitrate, e.Profiles, e.Status, e.Reason)
	}
}

// defaultCodecsDir is the app's codecs folder (06 §2: %LOCALAPPDATA%\isshoni\codecs; os.UserCacheDir is
// %LOCALAPPDATA% on Windows), or "" without a user cache folder.
func defaultCodecsDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(base, "isshoni", "codecs")
}
