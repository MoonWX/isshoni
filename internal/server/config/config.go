// Package config loads the server configuration (docs/m1/04-server-platform.md §4) and owns the data directory
// (§5): its layout (Paths), serve's startup checks (SetPrivateUmask, PrepareDataDir, the container data-volume check,
// ApplyMemoryLimit), the data-directory lock (LockDataDir) and secrets.json (OpenSecrets, SecretStore). A problem
// there that a restart can't fix is an *OperatorError wrapping ErrNeedsOperator, on which serve exits 78.
//
// Values come from four sources, highest first: command-line flags, ISSHONI_* environment variables, the TOML file
// and the built-in defaults (policy keys have a fifth, the admin UI setting in the database, §4.6). Every key is
// declared once in keys.go; that registry drives the TOML decoder, the env and flag names, validation, `isshoni
// config print`, `isshoni config example` and `isshoni serve --help`. Other docs add keys only there.
//
// Only cmd/isshoni, the wiring (internal/server), httpapi, tlsmgr, ops and push import this package (04 §2); every
// other package takes a plain option struct that the wiring fills.
package config

import (
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"
)

// DefaultPath is the config file read when neither --config nor ISSHONI_CONFIG names one. A missing file there is
// fine (Docker runs on env only); a missing file at an explicit path is an error.
const DefaultPath = "/etc/isshoni/isshoni.toml"

// Environment variables that are not keys but are read directly (§4.3 "Env-only switches").
const (
	EnvConfig             = "ISSHONI_CONFIG"               // the config file path
	EnvInContainer        = "ISSHONI_IN_CONTAINER"         // "1": running in 06's container image (also detected, §5.1)
	EnvAllowEphemeralData = "ISSHONI_ALLOW_EPHEMERAL_DATA" // "1": skip the container data-volume check (tests only)
)

// TLSMode is how the server gets its certificate (§8.1).
type TLSMode string

const (
	TLSAuto   TLSMode = "auto"   // Let's Encrypt for domain
	TLSIP     TLSMode = "ip"     // Let's Encrypt IP certificate for the public IP
	TLSManual TLSMode = "manual" // tls.cert_file and tls.key_file
	TLSOff    TLSMode = "off"    // plain HTTP on listen.http behind the operator's own HTTPS proxy
)

// Duration is a time.Duration written in Go syntax ("10s", "168h") in TOML, env and flags.
type Duration struct{ time.Duration }

// UnmarshalText parses Go duration syntax.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// MarshalText writes Go duration syntax.
func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// Config is the effective server configuration. Load fills it; its fields are the values of the keys in keys.go
// (the toml tags are the key paths). Policy keys (§4.6) hold the registry's copy of 03's defaults when they are not
// set; only values with IsSet true are pinned into 03's settings.
type Config struct {
	Domain          string   `toml:"domain"`
	PublicIP        string   `toml:"public_ip"`
	PublicIPv6      string   `toml:"public_ipv6"`
	PublicURL       string   `toml:"public_url"`
	DataDir         string   `toml:"data_dir"`
	ShutdownTimeout Duration `toml:"shutdown_timeout"`

	Listen       Listen       `toml:"listen"`
	TLS          TLS          `toml:"tls"`
	Network      Network      `toml:"network"`
	Registration Registration `toml:"registration"`
	Clients      Clients      `toml:"clients"`
	Limits       Limits       `toml:"limits"`
	SFU          SFU          `toml:"sfu"`
	Push         Push         `toml:"push"`
	Metrics      Metrics      `toml:"metrics"`
	Log          Log          `toml:"log"`
	Updates      Updates      `toml:"updates"`

	sources  map[string]Source // by key path; a key missing here has its default
	problems []Problem         // from the last Load: errors and warnings
	file     string            // the config file looked at ("" for LoadFlags)
	fileRead bool              // whether that file existed and was read
}

// Listen holds the listener addresses ([listen]).
type Listen struct {
	HTTPS       string `toml:"https"`
	HTTP        string `toml:"http"`
	ICEUDP      string `toml:"ice_udp"`
	ICETCP      string `toml:"ice_tcp"`
	AdminSocket string `toml:"admin_socket"`
}

// TLS is the [tls] section.
type TLS struct {
	Mode        TLSMode `toml:"mode"` // "" = derived; use EffectiveTLSMode
	ACMEEmail   string  `toml:"acme_email"`
	ACMECA      string  `toml:"acme_ca"`
	ACMECARoot  string  `toml:"acme_ca_root"`
	ACMEStaging bool    `toml:"acme_staging"`
	CertFile    string  `toml:"cert_file"`
	KeyFile     string  `toml:"key_file"`
	HSTS        bool    `toml:"hsts"`
}

// Network is the [network] section.
type Network struct {
	STUNServers       []string `toml:"stun_servers"`
	IPv6              bool     `toml:"ipv6"`
	ExcludeInterfaces []string `toml:"exclude_interfaces"`
	IncludeLoopback   bool     `toml:"include_loopback"`
	UDPBufferBytes    int      `toml:"udp_buffer_bytes"`
	// TrustedProxies is the effective value: Load fills in the loopback default of §4.4 (off mode with a loopback
	// listen.http and the key not set).
	TrustedProxies []netip.Prefix `toml:"trusted_proxies"`
}

// Registration is the [registration] section (a policy key).
type Registration struct {
	Mode string `toml:"mode"`
}

// Clients is the [clients] section (a policy key).
type Clients struct {
	MinVersion string `toml:"min_version"`
}

// Limits is the [limits] section: four policy keys and two boot-time guards.
type Limits struct {
	MaxParticipantsPerRoom     int `toml:"max_participants_per_room"`
	MaxSharesPerRoom           int `toml:"max_shares_per_room"`
	TransferAlertGB            int `toml:"transfer_alert_gb"`
	MaxBitrateKbps             int `toml:"max_bitrate_kbps"`
	WSHandshakesPerIPPerMinute int `toml:"ws_handshakes_per_ip_per_minute"`
	ConnsPerIP                 int `toml:"conns_per_ip"`
}

// SFU is the [sfu] section (02).
type SFU struct {
	PauseUnwatchedLayers bool `toml:"pause_unwatched_layers"`
}

// Push is the [push] section.
type Push struct {
	Enabled bool   `toml:"enabled"`
	Subject string `toml:"subject"`
}

// Metrics is the [metrics] section.
type Metrics struct {
	Enabled bool   `toml:"enabled"`
	Listen  string `toml:"listen"`
	PProf   bool   `toml:"pprof"`
}

// Log is the [log] section.
type Log struct {
	Level  string `toml:"level"`
	Format string `toml:"format"`
}

// Updates is the [updates] section.
type Updates struct {
	ReleaseCheck bool   `toml:"release_check"`
	ReleaseURL   string `toml:"release_url"`
}

// SourceKind says where a value came from.
type SourceKind string

const (
	SourceDefault SourceKind = "default"
	SourceFile    SourceKind = "file"
	SourceEnv     SourceKind = "env"
	SourceFlag    SourceKind = "flag"
)

// Source is where a value came from.
type Source struct {
	Kind   SourceKind `json:"kind"`
	File   string     `json:"file,omitempty"`   // for "file"
	Line   int        `json:"line,omitempty"`   // for "file"; 0 when unknown
	Column int        `json:"column,omitempty"` // for "file": set for syntax errors and unknown keys
	Name   string     `json:"name,omitempty"`   // env variable ("ISSHONI_TLS_MODE") or flag ("--tls.mode")
}

// String renders the source as in the §4.5 messages: "default", "file /etc/isshoni/isshoni.toml:7",
// "env ISSHONI_TLS_MODE", "flag --tls.mode".
func (s Source) String() string {
	switch s.Kind {
	case SourceFile:
		out := "file " + s.File
		if s.Line > 0 {
			out += ":" + strconv.Itoa(s.Line)
			if s.Column > 0 {
				out += ":" + strconv.Itoa(s.Column)
			}
		}
		return out
	case SourceEnv, SourceFlag:
		return string(s.Kind) + " " + s.Name
	case "":
		return string(SourceDefault)
	default:
		return string(s.Kind)
	}
}

// Warnings returns the warnings found by Load. Errors come back from Load as a
// *ValidationError.
func (c *Config) Warnings() []Problem {
	var out []Problem
	for _, p := range c.problems {
		if p.Severity == SeverityWarning {
			out = append(out, p)
		}
	}
	return out
}

// Problems returns every problem of the last Load, errors first, then warnings.
func (c *Config) Problems() []Problem { return slices.Clone(c.problems) }

// Source returns where the value of key (a dotted path such as "tls.mode") came from. Unknown keys and keys that
// were not set report the default.
func (c *Config) Source(key string) Source {
	if s, ok := c.sources[key]; ok {
		return s
	}
	return Source{Kind: SourceDefault}
}

// IsSet reports whether key was set by a flag, an env variable or the file (its source is not the default). The
// wiring pins exactly the policy keys for which it is true (§4.6).
func (c *Config) IsSet(key string) bool { return c.Source(key).Kind != SourceDefault }

// File returns the config file path that was looked at and whether it was read. The path is "" after LoadFlags.
func (c *Config) File() (path string, read bool) { return c.file, c.fileRead }

// EffectiveTLSMode is tls.mode if set; else auto when domain is set; else ip (§4.4).
func (c *Config) EffectiveTLSMode() TLSMode {
	switch {
	case c.TLS.Mode != "":
		return c.TLS.Mode
	case c.Domain != "":
		return TLSAuto
	default:
		return TLSIP
	}
}

// isLoopbackListen reports whether a host:port listener address binds only a loopback address ("127.0.0.1:8080",
// "[::1]:8080", "localhost:8080"). An empty host (":8080") binds every interface and is not loopback.
func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	return isLoopbackHost(host)
}

// isLoopbackHost reports whether host is "localhost" or a loopback address literal (brackets allowed).
func isLoopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.Unmap().IsLoopback()
}
