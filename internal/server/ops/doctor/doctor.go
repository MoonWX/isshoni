// Package doctor is the server's diagnostics (docs/m1/04-server-platform.md §13): the checks behind `isshoni
// doctor`, the admin page's "Run doctor" and the report the dashboard summarizes.
//
// Run runs the checks of 04 §13.2 against an Env and returns an api.DoctorReport. The same code serves two modes
// (04 §13.1):
//
//   - with a running server (Env.Live is set): the checks run inside the server's process, so they read the
//     server's own certificate state, public addresses, listeners and socket buffers from its status, and never
//     probe the network for what the server already knows;
//   - offline (Env.Live is nil): `isshoni doctor` while the server is stopped. The checks that need live state
//     (tls, transfer, release) are skipped, public_ip runs the detection itself, ports tries to bind each port, and
//     schema reads the database file through DBFiles without ever writing to it.
//
// Every check yields one api.DoctorCheck with a status, a stable code, the params of that code, and for a warning
// or failure a fix code. The English message and fix are rendered from the code and the params alone
// (messages_en.go), which is what lets the web client render the same report from its own catalog (05).
//
// Statuses: ok (checked, fine, or nothing to check in this configuration), info (a fact the operator should read;
// never counted as a problem), warn, fail, and skip (the check could not run: it needs a running server, the
// platform can't answer, or the run was interrupted).
//
// Everything the checks ask of the machine or the network goes through Env, so tests answer with fakes and make
// no network call. bandwidth.go has the calculator of 04 §13.4, render.go the CLI's text output.
//
// Imports (04 §2): config, logx, version, netx and internal/protocol/api; never store, tlsmgr, auth or the SFU.
// What doctor needs from 03's store arrives as function values (DBFiles), and the certificate state as part of
// the server's status, with tlsmgr's fix text for the last error next to it (Env.TLSLastError).
package doctor

import (
	"context"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/user"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/netx"
	"github.com/MoonWX/isshoni/internal/version"
)

// Env is what doctor's checks see of the machine, the network and the server. Every part can be faked. The fields
// of 04 §13.1 come first; the ones below them were added while building the checks, and each has a real default.
type Env struct {
	// Config is the loaded configuration, also when it has errors (config.Load still fills it in). Required.
	Config *config.Config
	// Live is the running server's status (the admin socket's GET /v1/status). nil means that no server is
	// reachable: the offline mode.
	Live *api.ServerStatus
	// FS is the machine's filesystem from its root, for /proc, /sys and /etc. nil means os.DirFS("/").
	FS fs.FS
	// Resolver resolves the domain for the dns check. nil means net.DefaultResolver.
	Resolver netx.Resolver
	// STUN asks STUN servers for the public address (offline public_ip). nil means netx.NewSTUNClient(Resolver).
	STUN netx.STUNClient
	// HTTP sends the clock check's one GET of the ACME directory, whose Date header is compared with the clock.
	// nil means a client of doctor's own with a 5 s timeout that trusts tls.acme_ca_root when it is set.
	HTTP *http.Client
	// Adjtimex reads the kernel's clock-sync flag (Linux). An error means that the state is unknown: EPERM and
	// ENOSYS are what systemd's ProtectClock= and a container's seccomp profile answer (04 §13.2 clock). nil means
	// the real call on Linux, and "unknown" elsewhere.
	Adjtimex func() (unsynced bool, err error)
	// DB carries 03's file-level store functions for the offline schema check, and LatestSchemaVersion for both
	// modes. A nil function makes the check skip what it can't know.
	DB DBFiles
	// Now is the clock. nil means time.Now.
	Now func() time.Time
	// GOOS is the operating system the checks are for. "" means runtime.GOOS.
	GOOS string
	// UID is the uid the checks run as: the service's uid inside the server, the caller's in the CLI. The caller
	// must set it (os.Getuid()); the zero value is root.
	UID int

	// User is the name of that user for the report's env.user. "" means: looked up from UID.
	User string
	// Host is the machine as the container checks see it (container runtime, mount table). The zero value is the
	// running process.
	Host config.Host
	// Interfaces lists the network interfaces. nil means netx.SystemInterfaces().
	Interfaces netx.InterfaceLister
	// Bind tries to bind a listener address and releases it at once: the offline ports check. network is "tcp" or
	// "udp". nil means a real bind.
	Bind func(ctx context.Context, network, addr string) error
	// CAA returns the CAA records that apply to a domain name, or none (the dns check in auto mode). nil means a
	// lookup through the resolvers of /etc/resolv.conf (caa.go).
	CAA func(ctx context.Context, name string) ([]CAARecord, error)
	// DiskFree returns the bytes free for an unprivileged user on the filesystem of dir. nil means statfs on Linux
	// and macOS, and "unknown" elsewhere.
	DiskFree func(dir string) (uint64, error)
	// NoFile returns the soft limit of open files (RLIMIT_NOFILE). nil means the real one where there is one.
	NoFile func() (uint64, error)
	// LookupUser returns the uid of the user with this name, and whether there is one. Offline, the data_dir and
	// secrets checks find the service's user with it (ServiceUser, which 06's installer creates), so that they
	// compare owners with the user the server runs as and not with whoever runs doctor. nil means the system's
	// user database (os/user).
	LookupUser func(name string) (uid int, ok bool)
	// TLSLastError is the fix text of the certificate's last error as the server wrote it
	// (tlsmgr.Status.LastError, 04 §8.7), which api.ServerStatus leaves out. It names what doctor can't know from
	// the code alone: the address a domain points to, the end of a rate limit, the CA's own words, the file that
	// can't be read. The wiring fills it from the tlsmgr.Status that Live.TLS was made of; the tls check prints
	// it as the fix when its fix code is Live.TLS.LastErrorCode. "" means: the code's text in messages_en.go.
	TLSLastError string
}

// DBFiles carries 03's file-level store functions, which never migrate or write the source file. doctor and ops
// may not import store (04 §2), so cmd/isshoni and the wiring fill it; ops's offline backup and restore validation
// (04 §12.3, §12.4) use the same struct.
type DBFiles struct {
	LatestSchemaVersion func() int                                               // store.LatestSchemaVersion
	Inspect             func(ctx context.Context, dbPath string) (DBInfo, error) // store.InspectFile (backupDir bound)
	Backup              func(ctx context.Context, srcPath, dstPath string) error // store.BackupFile
}

// DBInfo is store.FileInfo, copied field by field.
type DBInfo struct {
	SchemaVersion  int
	LastAppVersion string
	HistoryOK      bool
	Integrity      error  // PRAGMA integrity_check
	TooNewBackup   string // SchemaTooNewError.Backup, for the restore command of 04 §6.1 step 4
	// TooNewMessage is SchemaTooNewError.Error() when the file's schema is newer than this binary: 03's one
	// message for the operator, which the schema check prints as it is. "" otherwise; the check then writes the
	// same facts itself.
	TooNewMessage string
}

// The check ids of 04 §13.2, in table order. The project site has one troubleshooting anchor per id (06 §10.4).
const (
	idConfig       = "config"
	idDataDir      = "data_dir"
	idSecrets      = "secrets"
	idSchema       = "schema"
	idPublicIP     = "public_ip"
	idDNS          = "dns"
	idTLS          = "tls"
	idClock        = "clock"
	idUDPBuffers   = "udp_buffers"
	idPorts        = "ports"
	idFirewallHint = "firewall_hint"
	idContainer    = "container"
	idLANPrivacy   = "lan_privacy"
	idTransfer     = "transfer"
	idRelease      = "release"
	idNofile       = "nofile"
	idBandwidth    = "bandwidth"
)

// check is one row of 04 §13.2.
type check struct {
	id string
	// localOnly marks a check that only looks at this machine and says nothing about reachability from outside.
	localOnly bool
	run       func(ctx context.Context, r *run) result
}

// checks returns the checks in the order of 04 §13.2.
func checks() []check {
	return []check{
		{id: idConfig, run: checkConfig},
		{id: idDataDir, run: checkDataDir},
		{id: idSecrets, run: checkSecrets},
		{id: idSchema, run: checkSchema},
		{id: idPublicIP, run: checkPublicIP},
		{id: idDNS, run: checkDNS},
		{id: idTLS, run: checkTLS},
		{id: idClock, run: checkClock},
		{id: idUDPBuffers, run: checkUDPBuffers},
		{id: idPorts, localOnly: true, run: checkPorts},
		{id: idFirewallHint, run: checkFirewallHint},
		{id: idContainer, run: checkContainer},
		{id: idLANPrivacy, run: checkLANPrivacy},
		{id: idTransfer, run: checkTransfer},
		{id: idRelease, run: checkRelease},
		{id: idNofile, run: checkNofile},
		{id: idBandwidth, run: checkBandwidth},
	}
}

// CheckIDs returns the check ids of 04 §13.2 in table order: what `isshoni doctor --list-checks` prints and
// `--only` accepts.
func CheckIDs() []string {
	all := checks()
	ids := make([]string, len(all))
	for i, c := range all {
		ids[i] = c.id
	}
	return ids
}

// params are the values of a code's message: JSON-friendly (strings, numbers, booleans and lists of strings), with
// camelCase keys. They carry everything the message and the fix say, so the web client can render both from its
// catalog.
type params map[string]any

// result is what a check found.
type result struct {
	status  api.DoctorStatus
	code    string
	params  params
	fixCode string
}

func okResult(code string, p params) result {
	return result{status: api.DoctorStatusOK, code: code, params: p}
}
func infoResult(code string, p params) result {
	return result{status: api.DoctorStatusInfo, code: code, params: p}
}
func skipResult(code string, p params) result {
	return result{status: api.DoctorStatusSkip, code: code, params: p}
}

func warnResult(code string, p params, fixCode string) result {
	return result{status: api.DoctorStatusWarn, code: code, params: p, fixCode: fixCode}
}

func failResult(code string, p params, fixCode string) result {
	return result{status: api.DoctorStatusFail, code: code, params: p, fixCode: fixCode}
}

// withFix adds a fix to a result that is no warning or failure (the public_ip.ephemeral advice).
func (r result) withFix(fixCode string) result {
	r.fixCode = fixCode
	return r
}

// run is one doctor run: the Env with its defaults filled in, and what several checks share.
type run struct {
	env  Env
	cfg  *config.Config
	live *api.ServerStatus
	in   api.BandwidthInput
	host api.DoctorEnv

	ifacesOnce sync.Once
	ifaces     []netx.Interface

	publicOnce sync.Once
	public     publicState
}

// Run runs the checks and returns the report. only names the checks to run; nil or empty runs all of them, and an
// id that is not one of CheckIDs is ignored (the CLI and the admin socket refuse such a request before they get
// here). in is the session the bandwidth check estimates; its zero value is DefaultBandwidthInput.
//
// The report's mode is "server" when env.Live is set and "cli-offline" otherwise; the admin socket's handler
// changes it to "cli-with-server" for a report it hands to the CLI. Run never fails: a check that can't find out
// says so in its result. When ctx ends, the checks that have not run yet are skipped.
func Run(ctx context.Context, env Env, in api.BandwidthInput, only []string) api.DoctorReport {
	r := newRun(env, in)
	report := api.DoctorReport{
		Schema:  api.DoctorReportSchema,
		Version: version.Version(),
		RanAt:   r.now().UTC(),
		Mode:    api.DoctorModeCLIOffline,
		Env:     r.host,
		Checks:  []api.DoctorCheck{},
	}
	if r.live != nil {
		report.Mode = api.DoctorModeServer
		if r.live.Version != "" {
			report.Version = r.live.Version
		}
	}
	for _, c := range checks() {
		if len(only) > 0 && !slices.Contains(only, c.id) {
			continue
		}
		started := r.now()
		var res result
		if ctx.Err() != nil {
			res = skipResult(codeSkipInterrupted, nil)
		} else {
			res = c.run(ctx, r)
		}
		dc := api.DoctorCheck{
			ID:         c.id,
			Status:     res.status,
			Code:       res.code,
			Params:     res.params,
			Message:    messageEN(res.code, res.params, r.host),
			FixCode:    res.fixCode,
			LocalOnly:  c.localOnly,
			DurationMs: max(r.now().Sub(started).Milliseconds(), 0),
		}
		if len(dc.Params) == 0 {
			dc.Params = nil
		}
		if res.fixCode != "" {
			dc.Fix = fixEN(res.fixCode, res.params, r.host)
		}
		report.Checks = append(report.Checks, dc)
		switch res.status {
		case api.DoctorStatusOK:
			report.Summary.OK++
		case api.DoctorStatusWarn:
			report.Summary.Warn++
		case api.DoctorStatusFail:
			report.Summary.Fail++
		case api.DoctorStatusSkip:
			report.Summary.Skip++
		case api.DoctorStatusInfo:
			report.Summary.Info++
		}
		if c.id == idBandwidth {
			est := Bandwidth(r.in)
			report.Bandwidth = &est
		}
	}
	return report
}

// newRun fills in the defaults of env and collects the facts about the host.
func newRun(env Env, in api.BandwidthInput) *run {
	if env.Config == nil {
		panic("doctor: Run: Env.Config is nil")
	}
	if env.FS == nil {
		env.FS = os.DirFS("/")
	}
	if env.Resolver == nil {
		env.Resolver = net.DefaultResolver
	}
	if env.STUN == nil {
		env.STUN = netx.NewSTUNClient(env.Resolver)
	}
	if env.Now == nil {
		env.Now = time.Now
	}
	if env.GOOS == "" {
		env.GOOS = runtime.GOOS
	}
	if env.Interfaces == nil {
		env.Interfaces = netx.SystemInterfaces()
	}
	if env.Adjtimex == nil && env.GOOS == runtime.GOOS {
		env.Adjtimex = adjtimex // nil where the platform has none
	}
	if env.DiskFree == nil {
		env.DiskFree = diskFree
	}
	if env.NoFile == nil {
		env.NoFile = noFile
	}
	if env.Bind == nil {
		env.Bind = bind
	}
	if env.LookupUser == nil {
		env.LookupUser = lookupUser
	}
	r := &run{env: env, cfg: env.Config, live: env.Live, in: in}
	if r.in == (api.BandwidthInput{}) {
		r.in = DefaultBandwidthInput()
	}
	if env.CAA == nil {
		r.env.CAA = (&caaClient{fs: env.FS}).lookup
	}
	r.host = r.hostEnv()
	return r
}

func (r *run) now() time.Time { return r.env.Now() }

// offline reports the mode without a running server (04 §13.1).
func (r *run) offline() bool { return r.live == nil }

// tlsMode is the effective tls.mode.
func (r *run) tlsMode() config.TLSMode { return r.cfg.EffectiveTLSMode() }

// inContainer reports whether the checks run in a container.
func (r *run) inContainer() bool { return r.host.Container != api.ContainerKindNone }

// readFile returns the trimmed content of a machine file ("proc/sys/net/core/rmem_max"), and whether it could be
// read.
func (r *run) readFile(name string) (string, bool) {
	b, err := fs.ReadFile(r.env.FS, name)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

// readInt returns the number in a machine file.
func (r *run) readInt(name string) (int64, bool) {
	s, ok := r.readFile(name)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

// exists reports whether a machine file or directory exists.
func (r *run) exists(name string) bool {
	_, err := fs.Stat(r.env.FS, name)
	return err == nil
}

// hostEnv describes the host for the report (04 §13.5).
func (r *run) hostEnv() api.DoctorEnv {
	e := api.DoctorEnv{
		OS:        r.env.GOOS,
		Arch:      runtime.GOARCH,
		Container: api.ContainerKind(r.env.Host.Container()),
		Provider:  api.CloudProviderUnknown,
		User:      r.env.User,
	}
	if k, ok := r.readFile("proc/sys/kernel/osrelease"); ok {
		e.Kernel = k
	} else if r.env.GOOS == runtime.GOOS {
		e.Kernel = kernelRelease()
	}
	// sd_booted(3): systemd is the init system when this directory exists.
	e.Systemd = r.exists("run/systemd/system")
	if dmi, err := fs.Sub(r.env.FS, strings.TrimPrefix(netx.DMIDir, "/")); err == nil {
		e.Provider = netx.DetectCloudProvider(dmi)
	}
	if e.User == "" {
		e.User = userName(r.env.UID)
	}
	return e
}

// userName returns the name of the user with this uid, or "uid N" when it has none.
func userName(uid int) string {
	if uid < 0 { // Windows
		if u, err := user.Current(); err == nil && u.Username != "" {
			return u.Username
		}
		return "unknown"
	}
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil && u.Username != "" {
		return u.Username
	}
	return "uid " + strconv.Itoa(uid)
}

// lookupUser asks the system's user database for the uid of a user. A user without a numeric uid (Windows) counts
// as none.
func lookupUser(name string) (uid int, ok bool) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, false
	}
	uid, err = strconv.Atoi(u.Uid)
	return uid, err == nil && uid >= 0
}

// interfaces lists the network interfaces once per run. A list that can't be read is empty.
func (r *run) interfaces() []netx.Interface {
	r.ifacesOnce.Do(func() {
		r.ifaces, _ = r.env.Interfaces.Interfaces()
	})
	return r.ifaces
}

// ifaceOf returns the name of the interface that holds a, or "".
func (r *run) ifaceOf(a netip.Addr) string {
	if !a.IsValid() {
		return ""
	}
	for _, it := range r.interfaces() {
		for _, ia := range it.Addrs {
			if ia.Addr.WithZone("").Unmap() == a.WithZone("").Unmap() {
				return it.Name
			}
		}
	}
	return ""
}

// listener is one port the server listens on, from the config.
type listener struct {
	key     string // the config key, "listen.https"
	network string // "tcp" | "udp"
	addr    string // the configured address, ":443"; "" when the key is empty (the listener is off)
}

// label is the listener as the texts name it: "443/tcp".
func (l listener) label() string {
	return portOf(l.addr, l.defaultPort()) + "/" + l.network
}

// defaultPort is the port of the key's default, for the label of a listener that is turned off.
func (l listener) defaultPort() string {
	switch l.key {
	case "listen.https":
		return "443"
	case "listen.http":
		return "80"
	default:
		return "7882"
	}
}

// portOf returns the port of a "host:port" address, or fallback.
func portOf(addr, fallback string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil && port != "" {
		return port
	}
	return fallback
}

// listeners returns the ports this configuration serves (04 §7.1): 443 and 80 with a certificate of its own, the
// plain HTTP port behind a proxy in off mode, and the two media ports. One that the config turns off has an empty
// addr.
func (r *run) listeners() []listener {
	var out []listener
	if r.tlsMode() != config.TLSOff {
		out = append(out, listener{"listen.https", "tcp", r.cfg.Listen.HTTPS})
	}
	return append(out,
		listener{"listen.http", "tcp", r.cfg.Listen.HTTP},
		listener{"listen.ice_udp", "udp", r.cfg.Listen.ICEUDP},
		listener{"listen.ice_tcp", "tcp", r.cfg.Listen.ICETCP},
	)
}

// publicPorts returns the ports that friends must reach from outside, as the fix texts list them: TCP and UDP port
// numbers in the order of 04 §13.2 ("TCP 80, 443, 7882 and UDP 7882"). In off mode the web ports belong to the
// proxy in front and are left out.
func (r *run) publicPorts() (tcp, udp []string) {
	for _, l := range r.listeners() {
		if l.addr == "" || (r.tlsMode() == config.TLSOff && l.key == "listen.http") {
			continue
		}
		port := portOf(l.addr, l.defaultPort())
		if l.network == "udp" {
			udp = append(udp, port)
		} else if !slices.Contains(tcp, port) {
			tcp = append(tcp, port)
		}
	}
	slices.SortFunc(tcp, func(a, b string) int {
		x, _ := strconv.Atoi(a)
		y, _ := strconv.Atoi(b)
		return x - y
	})
	return tcp, udp
}
