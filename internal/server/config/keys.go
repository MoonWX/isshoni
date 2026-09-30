package config

import (
	"net/netip"
	"slices"
	"strings"
	"time"
)

// The key registry (04 §4.2, §4.3). Every config key of M1 is declared here once, with its path, default, kind,
// help, policy flag and consumer; other docs add keys only here (01's guard, 02's sfu.pause_unwatched_layers, 03's
// policy settings). The registry drives the TOML file, the env and flag names, validation, `isshoni config
// print`, `isshoni config example` and `isshoni serve --help`. This file is owned by the config slice (S15); later
// changes go through the integrator (docs/m1/README.md, "Shared files and their owners").

// Kind is the value type of a key.
type Kind string

const (
	KindString     Kind = "string"   // TOML string
	KindBool       Kind = "bool"     // TOML boolean; env and flags: true, false, 1 or 0
	KindInt        Kind = "int"      // TOML integer
	KindDuration   Kind = "duration" // TOML string in Go syntax: "10s", "168h"
	KindStringList Kind = "list"     // TOML array of strings; comma-separated in env and flags
	KindCIDRList   Kind = "cidrs"    // TOML array of CIDR strings; comma-separated in env and flags
)

// Key is one entry of the registry.
type Key struct {
	Path string // dotted TOML path: "tls.mode", or "domain" at the top level
	Kind Kind
	// Default is the built-in default, of the Go type that goes with Kind: string, bool, int, time.Duration,
	// []string or []netip.Prefix. listen.http and network.trusted_proxies have a derived default in off mode (§4.4);
	// DefaultNote says so.
	Default     any
	DefaultNote string
	Help        string   // one or two sentences for serve --help and config example
	Example     string   // a TOML value that config example shows (commented out) in place of an empty default
	Enum        []string // the values an enum key accepts; nil for other keys
	// Policy marks the keys an admin can also change in the admin UI (§4.6). Setting is 03's settings field (the JSON
	// name) that a set policy key pins. The registry's Default repeats 03's SettingsCache.Defaults() for docs and
	// config example only; a wiring test checks that they match.
	Policy   bool
	Setting  string
	Consumer string // the doc or package that reads the key (documentation only)
	Hidden   bool   // tests only: left out of config example and serve --help

	field func(*Config) any // a pointer to the key's field in Config
}

// EnvName is the key's environment variable: "ISSHONI_" plus the path in upper case with "." → "_"
// (tls.mode → ISSHONI_TLS_MODE).
func (k Key) EnvName() string {
	return "ISSHONI_" + strings.ToUpper(strings.ReplaceAll(k.Path, ".", "_"))
}

// FlagName is the key's command-line flag without the leading dashes: the path with "_" → "-" (tls.mode,
// public-ip, listen.admin-socket).
func (k Key) FlagName() string { return strings.ReplaceAll(k.Path, "_", "-") }

// Section is the TOML table the key lives in ("tls"), or "" for a top-level key.
func (k Key) Section() string {
	s, _, ok := strings.Cut(k.Path, ".")
	if !ok {
		return ""
	}
	return s
}

// Name is the key's name inside its section ("mode" for tls.mode).
func (k Key) Name() string { return k.Path[strings.LastIndexByte(k.Path, '.')+1:] }

// DefaultText is the default in env and flag syntax (lists comma-separated, strings unquoted), for help texts.
func (k Key) DefaultText() string { return formatText(k.Default) }

// DefaultTOML is the default as a TOML value.
func (k Key) DefaultTOML() string { return formatTOML(k.Default) }

// Keys returns the registry in its documentation order: the top-level keys, then one section after another.
func Keys() []Key {
	out := make([]Key, len(registry))
	for i, k := range registry {
		out[i] = k.clone()
	}
	return out
}

// Lookup returns the registry entry for a key path.
func Lookup(path string) (Key, bool) {
	i, ok := keyByPath[path]
	if !ok {
		return Key{}, false
	}
	return registry[i].clone(), true
}

// clone copies k so that changing the copy's Default or Enum can't change the registry.
func (k Key) clone() Key {
	k.Default = cloneValue(k.Default)
	k.Enum = slices.Clone(k.Enum)
	return k
}

// ReservedEnvNames are the ISSHONI_* names that are not config keys and that the server ignores without a warning:
// install.sh's, the release tooling's and the dev tasks' variables, plus ISSHONI_CONFIG (read as the config path).
// The list matches 06's; a new non-key ISSHONI_* name must be added to both (04 §4.2). The env-only switches
// ISSHONI_IN_CONTAINER and ISSHONI_ALLOW_EPHEMERAL_DATA are read by the server and are not warned about either.
func ReservedEnvNames() []string { return slices.Clone(reservedEnv) }

var reservedEnv = []string{
	EnvConfig,
	"ISSHONI_VERSION", // install.sh's target version and the Vite build version
	"ISSHONI_YES",
	"ISSHONI_NO_FIREWALL",
	"ISSHONI_DOWNLOAD_BASE",
	"ISSHONI_INSTALL_SOURCED",
	"ISSHONI_INSTALLER_VERSION",
	"ISSHONI_SNAPSHOT_VERSION",
	"ISSHONI_BIN",
	"ISSHONI_DEV_SERVER",
}

// envOnly are the env-only switches of §4.3: read directly, never warned about.
var envOnly = []string{EnvConfig, EnvInContainer, EnvAllowEphemeralData}

const releasesURL = "https://api.github.com/repos/MoonWX/isshoni/releases"

var (
	defaultSTUNServers       = []string{"stun.cloudflare.com:3478", "stun.l.google.com:19302"}
	defaultExcludeInterfaces = []string{"docker*", "br-*", "veth*", "virbr*", "cni*", "flannel*", "cali*", "kube-*"}
	// loopbackProxies is the effective network.trusted_proxies in off mode with a loopback listen.http (§4.4).
	loopbackProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
)

// offModeHTTP is the default of listen.http when tls.mode is "off" (§4.3).
const offModeHTTP = "127.0.0.1:8080"

// registry is every key of M1, in documentation order (04 §4.3).
var registry = []Key{
	// Top level.
	{
		Path: "domain", Kind: KindString, Default: "", Example: `"watch.example.com"`, Consumer: "tlsmgr, site",
		Help:  "The DNS name friends use. Empty: use the public IP.",
		field: func(c *Config) any { return &c.Domain },
	},
	{
		Path: "public_ip", Kind: KindString, Default: "auto", Consumer: "netx",
		Help:  `"auto" (from the interfaces, then STUN) or this server's public IPv4 or IPv6 address.`,
		field: func(c *Config) any { return &c.PublicIP },
	},
	{
		Path: "public_ipv6", Kind: KindString, Default: "auto", Consumer: "netx",
		Help:  `"auto", "off" or a public IPv6 address: extra IPv6 media candidates.`,
		field: func(c *Config) any { return &c.PublicIPv6 },
	},
	{
		Path: "public_url", Kind: KindString, Default: "", Example: `"https://watch.example.com"`, Consumer: "site",
		Help: `The URL friends open when tls.mode = "off" (behind your own HTTPS proxy). Required in that mode ` +
			`unless listen.http is a loopback address (dev).`,
		field: func(c *Config) any { return &c.PublicURL },
	},
	{
		Path: "data_dir", Kind: KindString, Default: "/var/lib/isshoni", Consumer: "all",
		Help:  "The data directory: database, secrets, certificates and backups.",
		field: func(c *Config) any { return &c.DataDir },
	},
	{
		Path: "shutdown_timeout", Kind: KindDuration, Default: 10 * time.Second, Consumer: "server",
		Help:  "The upper bound for a graceful shutdown.",
		field: func(c *Config) any { return &c.ShutdownTimeout },
	},

	// [listen]
	{
		Path: "listen.https", Kind: KindString, Default: ":443", Consumer: "netx, tlsmgr",
		Help:  `HTTPS and WSS, and ICE-TCP on the same port (split by the first byte). Ignored when tls.mode = "off".`,
		field: func(c *Config) any { return &c.Listen.HTTPS },
	},
	{
		Path: "listen.http", Kind: KindString, Default: ":80", DefaultNote: `"127.0.0.1:8080" when tls.mode = "off"`,
		Consumer: "tlsmgr, httpapi",
		Help:     `ACME HTTP-01 challenges and the redirect to HTTPS; the app itself when tls.mode = "off".`,
		field:    func(c *Config) any { return &c.Listen.HTTP },
	},
	{
		Path: "listen.ice_udp", Kind: KindString, Default: ":7882", Consumer: "netx",
		Help:  `The ICE UDP port for all media. "" turns UDP off.`,
		field: func(c *Config) any { return &c.Listen.ICEUDP },
	},
	{
		Path: "listen.ice_tcp", Kind: KindString, Default: ":7882", Consumer: "netx",
		Help:  `The ICE-TCP fallback port (needed when tls.mode = "off"). "" turns it off.`,
		field: func(c *Config) any { return &c.Listen.ICETCP },
	},
	{
		Path: "listen.admin_socket", Kind: KindString, Default: "/run/isshoni/admin.sock", Consumer: "ops, cli",
		Help:  "The admin socket of the isshoni CLI (at most 104 bytes).",
		field: func(c *Config) any { return &c.Listen.AdminSocket },
	},

	// [tls]
	{
		Path: "tls.mode", Kind: KindString, Default: "", Example: `"auto"`, Enum: []string{"auto", "ip", "manual", "off"},
		DefaultNote: "auto when domain is set, else ip", Consumer: "tlsmgr, site",
		Help:  "auto (Let's Encrypt for domain), ip (Let's Encrypt for the public IP), manual (your files) or off (your own HTTPS proxy).",
		field: func(c *Config) any { return &c.TLS.Mode },
	},
	{
		Path: "tls.acme_email", Kind: KindString, Default: "", Example: `"you@example.com"`, Consumer: "tlsmgr, push",
		Help:  "An optional contact address for the ACME account.",
		field: func(c *Config) any { return &c.TLS.ACMEEmail },
	},
	{
		Path: "tls.acme_ca", Kind: KindString, Default: "https://acme-v02.api.letsencrypt.org/directory", Consumer: "tlsmgr",
		Help:  "The ACME directory.",
		field: func(c *Config) any { return &c.TLS.ACMECA },
	},
	{
		Path: "tls.acme_staging", Kind: KindBool, Default: false, Consumer: "tlsmgr",
		Help:  "Use Let's Encrypt staging: untrusted certificates, for testing only.",
		field: func(c *Config) any { return &c.TLS.ACMEStaging },
	},
	{
		Path: "tls.acme_ca_root", Kind: KindString, Default: "", Consumer: "tlsmgr",
		Help:  "A PEM file trusted for the ACME server's own TLS. Testing only.",
		field: func(c *Config) any { return &c.TLS.ACMECARoot },
	},
	{
		Path: "tls.cert_file", Kind: KindString, Default: "", Example: `"/etc/isshoni/fullchain.pem"`, Consumer: "tlsmgr",
		Help:  `The certificate chain (PEM) when tls.mode = "manual".`,
		field: func(c *Config) any { return &c.TLS.CertFile },
	},
	{
		Path: "tls.key_file", Kind: KindString, Default: "", Example: `"/etc/isshoni/privkey.pem"`, Consumer: "tlsmgr",
		Help:  `The private key (PEM) when tls.mode = "manual".`,
		field: func(c *Config) any { return &c.TLS.KeyFile },
	},
	{
		Path: "tls.hsts", Kind: KindBool, Default: true, Consumer: "httpapi",
		Help:  "Send HSTS (only when serving a domain over TLS).",
		field: func(c *Config) any { return &c.TLS.HSTS },
	},

	// [network]
	{
		Path: "network.stun_servers", Kind: KindStringList, Default: defaultSTUNServers, Consumer: "netx",
		Help:  "STUN servers (host:port) for public IP detection only.",
		field: func(c *Config) any { return &c.Network.STUNServers },
	},
	{
		Path: "network.ipv6", Kind: KindBool, Default: true, Consumer: "netx",
		Help:  "Gather and advertise IPv6 media candidates when a global address exists.",
		field: func(c *Config) any { return &c.Network.IPv6 },
	},
	{
		Path: "network.exclude_interfaces", Kind: KindStringList, Default: defaultExcludeInterfaces, Consumer: "netx",
		Help:  "Interface name globs never used for media.",
		field: func(c *Config) any { return &c.Network.ExcludeInterfaces },
	},
	{
		Path: "network.include_loopback", Kind: KindBool, Default: false, Consumer: "netx",
		Help:  "Dev only: advertise 127.0.0.1 and ::1 media candidates.",
		field: func(c *Config) any { return &c.Network.IncludeLoopback },
	},
	{
		Path: "network.udp_buffer_bytes", Kind: KindInt, Default: 8388608, Consumer: "netx",
		Help:  "SO_RCVBUF and SO_SNDBUF requested on media sockets (1 MiB to 64 MiB); the sysctl limits must allow it.",
		field: func(c *Config) any { return &c.Network.UDPBufferBytes },
	},
	{
		Path: "network.trusted_proxies", Kind: KindCIDRList, Default: []netip.Prefix{},
		DefaultNote: `["127.0.0.0/8", "::1/128"] when tls.mode = "off" and listen.http is a loopback address`,
		Consumer:    "httpapi",
		Help:        `CIDRs allowed to set X-Forwarded-For and X-Forwarded-Proto (tls.mode = "off" only).`,
		field:       func(c *Config) any { return &c.Network.TrustedProxies },
	},

	// Policy keys (§4.6): also editable in the admin UI unless set here.
	{
		Path: "registration.mode", Kind: KindString, Default: "invite", Enum: []string{"invite", "approval", "closed"},
		Policy: true, Setting: "registrationMode", Consumer: "03",
		Help:  "Who can create an account: invite (an invite link), approval (anyone, then an admin approves) or closed.",
		field: func(c *Config) any { return &c.Registration.Mode },
	},
	{
		Path: "clients.min_version", Kind: KindString, Default: "", Example: `"0.3.0"`,
		Policy: true, Setting: "minClientVersion", Consumer: "01",
		Help:  `The oldest native app version (SemVer) allowed to connect; older apps show "Update the app". Empty: no minimum.`,
		field: func(c *Config) any { return &c.Clients.MinVersion },
	},
	{
		Path: "limits.max_participants_per_room", Kind: KindInt, Default: 0,
		Policy: true, Setting: "maxParticipantsPerRoom", Consumer: "01",
		Help:  "A soft limit of people per room. 0: no limit.",
		field: func(c *Config) any { return &c.Limits.MaxParticipantsPerRoom },
	},
	{
		Path: "limits.max_shares_per_room", Kind: KindInt, Default: 0,
		Policy: true, Setting: "maxSharesPerRoom", Consumer: "01",
		Help:  "A soft limit of shares per room. 0: no limit.",
		field: func(c *Config) any { return &c.Limits.MaxSharesPerRoom },
	},
	{
		Path: "limits.max_bitrate_kbps", Kind: KindInt, Default: 0,
		Policy: true, Setting: "maxShareBitrateKbps", Consumer: "01, 02",
		Help:  "A cap on any share's full layer in kbps (500 to 100000). 0: the quality presets' caps only.",
		field: func(c *Config) any { return &c.Limits.MaxBitrateKbps },
	},
	{
		Path: "limits.transfer_alert_gb", Kind: KindInt, Default: 0,
		Policy: true, Setting: "transferAlertGb", Consumer: "ops",
		Help:  "Alert admins at 80% and 100% of this monthly transfer (1 GB = 10^9 bytes). 0: off.",
		field: func(c *Config) any { return &c.Limits.TransferAlertGB },
	},
	// Guards (boot time, not policy).
	{
		Path: "limits.ws_handshakes_per_ip_per_minute", Kind: KindInt, Default: 20, Consumer: "01 (hub)",
		Help:  "Signaling handshakes before sign-in per client IP and minute. Raise it for LAN parties behind one IP.",
		field: func(c *Config) any { return &c.Limits.WSHandshakesPerIPPerMinute },
	},
	{
		Path: "limits.conns_per_ip", Kind: KindInt, Default: 256, Consumer: "netx",
		Help:  "Open TCP connections per source IP (an IPv4 address or an IPv6 /64) on the HTTPS and HTTP ports.",
		field: func(c *Config) any { return &c.Limits.ConnsPerIP },
	},

	// [updates]
	{
		Path: "updates.release_check", Kind: KindBool, Default: true,
		Policy: true, Setting: "updateCheck", Consumer: "ops",
		Help:  "Check GitHub once a day for a newer release and show it in the admin dashboard.",
		field: func(c *Config) any { return &c.Updates.ReleaseCheck },
	},
	{
		Path: "updates.release_url", Kind: KindString, Default: releasesURL, Hidden: true, Consumer: "ops",
		Help:  "The GitHub releases API URL. Tests only.",
		field: func(c *Config) any { return &c.Updates.ReleaseURL },
	},

	// [sfu] (02)
	{
		Path: "sfu.pause_unwatched_layers", Kind: KindBool, Default: true, Consumer: "02",
		Help:  "Ask sharers' browsers to pause the full-quality layer while nobody watches it.",
		field: func(c *Config) any { return &c.SFU.PauseUnwatchedLayers },
	},

	// [push]
	{
		Path: "push.enabled", Kind: KindBool, Default: true, Consumer: "push",
		Help:  `Web Push notifications ("Alex started sharing").`,
		field: func(c *Config) any { return &c.Push.Enabled },
	},
	{
		Path: "push.subject", Kind: KindString, Default: "", Example: `"mailto:you@example.com"`, Consumer: "push",
		DefaultNote: "mailto:<tls.acme_email> if set, else the public origin",
		Help:        "The VAPID subject: a mailto: or https: URL.",
		field:       func(c *Config) any { return &c.Push.Subject },
	},

	// [metrics]
	{
		Path: "metrics.enabled", Kind: KindBool, Default: false, Consumer: "ops",
		Help:  "Serve Prometheus metrics on metrics.listen.",
		field: func(c *Config) any { return &c.Metrics.Enabled },
	},
	{
		Path: "metrics.listen", Kind: KindString, Default: "127.0.0.1:9469", Consumer: "ops",
		Help:  "The metrics listener. Metrics are unauthenticated: keep it on loopback.",
		field: func(c *Config) any { return &c.Metrics.Listen },
	},
	{
		Path: "metrics.pprof", Kind: KindBool, Default: false, Consumer: "ops",
		Help:  "Also serve /debug/pprof/ on the metrics listener.",
		field: func(c *Config) any { return &c.Metrics.PProf },
	},

	// [log]
	{
		Path: "log.level", Kind: KindString, Default: "info", Enum: []string{"debug", "info", "warn", "error"},
		Consumer: "logx",
		Help:     "debug, info, warn or error.",
		field:    func(c *Config) any { return &c.Log.Level },
	},
	{
		Path: "log.format", Kind: KindString, Default: "auto", Enum: []string{"auto", "text", "json"}, Consumer: "logx",
		Help:  "auto (text on a terminal, else JSON lines), text or json.",
		field: func(c *Config) any { return &c.Log.Format },
	},
}

// keyByPath maps a key path to its index in registry.
var keyByPath = func() map[string]int {
	m := make(map[string]int, len(registry))
	for i, k := range registry {
		m[k.Path] = i
	}
	return m
}()

// sections are the TOML tables that hold keys.
var sections = func() map[string]bool {
	m := map[string]bool{}
	for _, k := range registry {
		if s := k.Section(); s != "" {
			m[s] = true
		}
	}
	return m
}()

// get returns the key's current value in c, as the Go type of its Kind.
func (k *Key) get(c *Config) any {
	switch p := k.field(c).(type) {
	case *string:
		return *p
	case *TLSMode:
		return string(*p)
	case *bool:
		return *p
	case *int:
		return *p
	case *Duration:
		return p.Duration
	case *[]string:
		return slices.Clone(*p)
	case *[]netip.Prefix:
		return slices.Clone(*p)
	default:
		panic("config: unsupported field type for " + k.Path)
	}
}

// set stores v, of the Go type of the key's Kind, in c.
func (k *Key) set(c *Config, v any) {
	switch p := k.field(c).(type) {
	case *string:
		*p = v.(string)
	case *TLSMode:
		*p = TLSMode(v.(string))
	case *bool:
		*p = v.(bool)
	case *int:
		*p = v.(int)
	case *Duration:
		p.Duration = v.(time.Duration)
	case *[]string:
		*p = slices.Clone(v.([]string))
	case *[]netip.Prefix:
		*p = slices.Clone(v.([]netip.Prefix))
	default:
		panic("config: unsupported field type for " + k.Path)
	}
}

func cloneValue(v any) any {
	switch v := v.(type) {
	case []string:
		return slices.Clone(v)
	case []netip.Prefix:
		return slices.Clone(v)
	default:
		return v
	}
}
