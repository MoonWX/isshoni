package doctor

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// The codes of doctor's results and their English texts (04 §13.2). A code is the key of a message template; the
// params of its result are the template's values. The texts here are the CLI's. The web client renders the same
// codes from its catalog (doctor.<code> and doctor.fix.<fixCode>, 05 §16.5), so a text may use nothing but the
// params of its result and the report's env, and a code is never renamed or reused once released.
//
// A message may have more than one line: the CLI indents the lines after the first. A text starts in lower case
// unless it starts with a name, and ends without a period, like the lines of 04 §13.2's examples; where 04 gives a
// whole sentence (the certificate fixes of §8.7, the notes of §13.2), the text is that sentence.

// Message codes. The part before the dot is the check's id, except for the skip codes, which any check may use.
const (
	codeSkipNeedsServer = "skip.needs_server" // only a running server knows this
	codeSkipInterrupted = "skip.interrupted"  // the run was cancelled before this check
	codeSkipUnsupported = "skip.unsupported"  // this platform can't answer; params: os
	codeSkipNotReported = "skip.not_reported" // the server's status (or this build) does not carry the fact

	codeConfigOK       = "config.ok"       // params: file, fileRead
	codeConfigWarnings = "config.warnings" // params: file, fileRead, errors, warnings, first, firstFix?
	codeConfigInvalid  = "config.invalid"  // as config.warnings

	codeDataDirOK               = "data_dir.ok"                // params: path, mode, freeBytes?
	codeDataDirNotCreated       = "data_dir.not_created"       // offline, before the first start; params: path
	codeDataDirMissing          = "data_dir.missing"           // a running server whose data_dir is gone; params: path
	codeDataDirUnreadable       = "data_dir.unreadable"        // params: path, error
	codeDataDirNotDir           = "data_dir.not_dir"           // params: path
	codeDataDirNotMounted       = "data_dir.not_mounted"       // in a container, not a volume; params: path, error?
	codeDataDirEphemeralAllowed = "data_dir.ephemeral_allowed" // the same, allowed by ISSHONI_ALLOW_EPHEMERAL_DATA
	codeDataDirWrongOwner       = "data_dir.wrong_owner"       // params: path, owner, uid, offline
	codeDataDirNotWritable      = "data_dir.not_writable"      // params: path, error, uid
	codeDataDirModeWide         = "data_dir.mode_wide"         // params: path, mode
	codeDataDirLowSpace         = "data_dir.low_space"         // params: path, freeBytes

	codeSecretsOK         = "secrets.ok"          // params: path, mode
	codeSecretsNotCreated = "secrets.not_created" // offline, before the first start; params: path
	codeSecretsMissing    = "secrets.missing"     // the database is there, its keys are not; params: path, offline
	codeSecretsUnreadable = "secrets.unreadable"  // params: path, error
	codeSecretsWrongOwner = "secrets.wrong_owner" // params: path, owner, uid
	codeSecretsCorrupt    = "secrets.corrupt"     // params: path, error
	codeSecretsModeWide   = "secrets.mode_wide"   // params: path, mode

	codeSchemaOK              = "schema.ok"               // params: version, binary?, path? (offline)
	codeSchemaNoDB            = "schema.no_db"            // offline, before the first start; params: path
	codeSchemaOlder           = "schema.older"            // the next start migrates it; params: path, version, binary
	codeSchemaNewer           = "schema.newer"            // params: path, version, binary, lastAppVersion?, backup?, storeMessage?
	codeSchemaHistoryMismatch = "schema.history_mismatch" // params: path, version, lastAppVersion?
	codeSchemaCorrupt         = "schema.corrupt"          // params: path, error
	codeSchemaUnreadable      = "schema.unreadable"       // params: path, error

	// The public_ip codes share: ipv4?, ipv4Method?, iface?, ipv6?, ipv6Method?, local?, nat.
	codePublicIPOK          = "public_ip.ok"
	codePublicIPNoIPv6      = "public_ip.no_ipv6"      // info: an IPv4 address only
	codePublicIPv6Only      = "public_ip.ipv6_only"    // no public IPv4
	codePublicIPEphemeral   = "public_ip.ephemeral"    // info: ip mode on AWS or GCP; params: provider
	codePublicIPPortForward = "public_ip.port_forward" // behind a home router; params: tcpPorts, udpPorts
	codePublicIPSymmetric   = "public_ip.symmetric"
	codePublicIPCGNAT       = "public_ip.cgnat"
	codePublicIPNone        = "public_ip.none" // params: siteIsAddress, running, error?

	// The dns codes share: domain, ip?, ipv6?, a?, aaaa?, caa? ("allows" | "none" | "unchecked").
	codeDNSOK            = "dns.ok"
	codeDNSNoDomain      = "dns.no_domain"      // the site is the IP address
	codeDNSProxy         = "dns.proxy"          // off mode: DNS points at the proxy
	codeDNSMissing       = "dns.missing"        // no A or AAAA record
	codeDNSWrong         = "dns.wrong"          // the records point elsewhere; params: others
	codeDNSAlsoElsewhere = "dns.also_elsewhere" // this server and others; params: others
	codeDNSNoA           = "dns.no_a"           // an AAAA record only, though the server has an IPv4 address
	codeDNSAAAAWrong     = "dns.aaaa_wrong"     // the AAAA record is not this server; params: others
	codeDNSCAAForbids    = "dns.caa_forbids"    // params: caaName
	codeDNSLookupFailed  = "dns.lookup_failed"  // the resolver failed; params: error
	codeDNSUnverified    = "dns.unverified"     // no public address to compare with

	// The tls codes share: mode, names?, name?, domain?, ip?, issuer?, notAfter?, nextRenewal?.
	codeTLSOK             = "tls.ok"
	codeTLSOff            = "tls.off"             // a proxy has the certificate
	codeTLSPending        = "tls.pending"         // info: the first order is under way
	codeTLSFailing        = "tls.failing"         // no certificate yet and an attempt failed; fix: the 04 §8.7 code
	codeTLSNoCert         = "tls.no_cert"         // no certificate after ten minutes; params: uptimeS
	codeTLSNoPublicIP     = "tls.no_public_ip"    // ip mode without an address to certify
	codeTLSRenewalFailing = "tls.renewal_failing" // the certificate is valid, its renewal fails
	codeTLSReloadFailed   = "tls.reload_failed"   // manual: new files were refused, the old pair serves
	codeTLSExpiresSoon    = "tls.expires_soon"    // manual: fewer than 14 days left; params: days
	codeTLSExpired        = "tls.expired"
	codeTLSNotLoaded      = "tls.not_loaded" // manual: the files can't be read or don't match

	codeClockOK       = "clock.ok"        // params: sync ("yes" | "no" | "unknown"), skewS?, host?, error?
	codeClockUnknown  = "clock.unknown"   // info: neither source could say
	codeClockUnsynced = "clock.unsynced"  // the kernel says no time daemon steers the clock
	codeClockSkew     = "clock.skew"      // warn from 30 s, fail above 5 min; params: skewS, host
	codeClockCertTime = "clock.cert_time" // the CA's certificate looks expired from here; params: host, now

	codeUDPBuffersOK      = "udp_buffers.ok"      // params: want, rmemMax?, wmemMax?, rcvBuf?, sndBuf?
	codeUDPBuffersLow     = "udp_buffers.low"     // the same
	codeUDPBuffersUDPOff  = "udp_buffers.udp_off" // listen.ice_udp is empty
	codeUDPBuffersUnknown = "udp_buffers.unknown" // Linux without the sysctl files and without a server

	codePortsListening    = "ports.listening"     // info; params: listening
	codePortsFree         = "ports.free"          // info, offline; params: free, unchecked?, uncheckedDenied?
	codePortsDisabled     = "ports.disabled"      // params: listening | free, disabled, disabledKeys
	codePortsNotListening = "ports.not_listening" // params: listening?, missing
	codePortsInUse        = "ports.in_use"        // offline; params: inUse, inUseKeys, free?

	codeFirewallHintOpenPorts = "firewall_hint.open_ports" // params: provider, tcpPorts?, udpPorts?, proxy?, dockerBridge?

	codeContainerNone     = "container.none"
	codeContainerDetected = "container.detected" // params: kind, network, tcpPorts?, udpPorts?

	codeLANPrivacyMacOS    = "lan_privacy.macos"
	codeLANPrivacyNotMacOS = "lan_privacy.not_macos"

	// The transfer codes share: month, egressBytes, ingressBytes, alertGb?, pct?, projectedEgressBytes?.
	codeTransferOK      = "transfer.ok"
	codeTransferNoLimit = "transfer.no_limit"
	codeTransferHigh    = "transfer.high" // 80 % of the limit and more
	codeTransferOver    = "transfer.over" // the limit and more

	codeReleaseOK             = "release.ok"          // params: version, latest
	codeReleaseNotChecked     = "release.not_checked" // params: version
	codeReleaseUpdate         = "release.update"      // params: version, latest, url?
	codeReleaseSecurityUpdate = "release.security_update"

	codeNofileOK  = "nofile.ok"  // info; params: limit, want, offline
	codeNofileLow = "nofile.low" // the same

	// The bandwidth codes share: people, sharing, hours, egressMediaMbps, egressWireMbps, transferPerSessionGb.
	codeBandwidthEstimate = "bandwidth.estimate"
	codeBandwidthNearNIC  = "bandwidth.near_nic" // params: iface, nicMbps
)

// Fix codes. The tls check also uses the ten codes of 04 §8.7 (checks_net.go) as fix codes.
const (
	fixConfigCheck = "config.fix_check"

	fixDataDirSudo   = "data_dir.fix_sudo"
	fixDataDirOwner  = "data_dir.fix_owner"
	fixDataDirNotDir = "data_dir.fix_not_dir"
	fixDataDirMount  = "data_dir.fix_mount"
	fixDataDirMode   = "data_dir.fix_mode"
	fixDataDirSpace  = "data_dir.fix_space"

	fixSecretsSudo    = "secrets.fix_sudo" //nolint:gosec // G101 false positive: a fix code, not a credential
	fixSecretsOwner   = "secrets.fix_owner"
	fixSecretsRestore = "secrets.fix_restore"
	fixSecretsMode    = "secrets.fix_mode"

	fixSchemaSudo          = "schema.fix_sudo"
	fixSchemaRestore       = "schema.fix_restore"        // 03's message and the restore command
	fixSchemaUpgrade       = "schema.fix_upgrade"        // no backup from before the upgrade
	fixSchemaSameBuild     = "schema.fix_same_build"     // a history that is another build's
	fixSchemaRestoreBackup = "schema.fix_restore_backup" // a damaged file

	fixPublicIPSet     = "public_ip.fix_set"
	fixPublicIPNAT     = "public_ip.fix_nat"
	fixPublicIPForward = "public_ip.fix_forward"
	fixPublicIPStatic  = "public_ip.fix_static_ip"

	fixDNSAddRecord = "dns.fix_add_record"
	fixDNSPointHere = "dns.fix_point_here"
	fixDNSAAAA      = "dns.fix_aaaa"
	fixDNSCAA       = "dns.fix_caa"

	fixTLSReplace = "tls.fix_replace"

	fixClockNTP = "clock.fix_ntp"

	fixUDPBuffersSysctl       = "udp_buffers.fix_sysctl"
	fixUDPBuffersSysctlReload = "udp_buffers.fix_sysctl_reload"
	fixUDPBuffersRestart      = "udp_buffers.fix_restart"

	fixPortsFree = "ports.fix_free"

	fixTransferReview = "transfer.fix_review"

	fixReleaseUpgrade = "release.fix_upgrade"

	fixNofileLimit = "nofile.fix_limit"

	fixBandwidthLink = "bandwidth.fix_link"
)

// text renders one message or fix from its params and the report's env.
type text func(p params, e api.DoctorEnv) string

// messageEN returns the English message of a code. A code without a text prints as itself; a test keeps that from
// happening.
func messageEN(code string, p params, e api.DoctorEnv) string {
	if f, ok := messagesEN[code]; ok {
		return f(p, e)
	}
	return code
}

// fixEN returns the English fix of a fix code, or "" when the code has none for these params. A certificate code
// that this build does not know (the tls check passes the server's code on as it is) gets the text of the general
// one, which points at the server's log.
func fixEN(code string, p params, e api.DoctorEnv) string {
	if f, ok := fixesEN[code]; ok {
		return f(p, e)
	}
	if strings.HasPrefix(code, "tls.") {
		return fixesEN[tlsACMEFailed](p, e)
	}
	return ""
}

// Codes returns every message code, sorted: the keys the web client's catalog needs under doctor.<code>.
func Codes() []string { return sortedKeys(messagesEN) }

// FixCodes returns every fix code, sorted: the keys under doctor.fix.<fixCode>.
func FixCodes() []string { return sortedKeys(fixesEN) }

func sortedKeys(m map[string]text) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// ---- reading params ----

func (p params) str(key string) string {
	switch v := p[key].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func (p params) num(key string) float64 {
	switch v := p[key].(type) {
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case float64:
		return v
	}
	return 0
}

func (p params) int(key string) int64 { return int64(p.num(key)) }

func (p params) bool(key string) bool {
	b, _ := p[key].(bool)
	return b
}

func (p params) list(key string) []string {
	switch v := p[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, len(v))
		for i, x := range v {
			out[i] = fmt.Sprint(x)
		}
		return out
	}
	return nil
}

func (p params) has(key string) bool {
	_, ok := p[key]
	return ok
}

// ---- formatting ----

// and joins a list for a sentence: "a", "a and b", "a, b and c".
func and(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// portsText names ports as the firewall texts do: "TCP 80, 443, 7882 and UDP 7882".
func portsText(p params) string {
	var parts []string
	if tcp := p.list("tcpPorts"); len(tcp) > 0 {
		parts = append(parts, "TCP "+strings.Join(tcp, ", "))
	}
	if udp := p.list("udpPorts"); len(udp) > 0 {
		parts = append(parts, "UDP "+strings.Join(udp, ", "))
	}
	return strings.Join(parts, " and ")
}

// sizeText prints a byte count in GB or MB of 10⁹ and 10⁶ bytes, as providers bill them.
func sizeText(n int64) string {
	if n >= 1_000_000_000 {
		return strconv.FormatFloat(float64(n)/1e9, 'f', 1, 64) + " GB"
	}
	return strconv.FormatFloat(float64(n)/1e6, 'f', 0, 64) + " MB"
}

// timeText prints a wire timestamp for people, in UTC to the minute; dateText only its day.
func timeText(wire string) string {
	t, err := time.Parse(time.RFC3339, wire)
	if err != nil {
		return wire
	}
	return t.UTC().Format("2006-01-02 15:04") + " UTC"
}

func dateText(wire string) string {
	t, err := time.Parse(time.RFC3339, wire)
	if err != nil {
		return wire
	}
	return t.UTC().Format(time.DateOnly)
}

// plural is "1 warning", "3 warnings".
func plural(n int64, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.FormatInt(n, 10) + " " + noun + "s"
}

// number prints a float without a trailing ".0": 2, 2.5.
func number(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func inContainer(e api.DoctorEnv) bool {
	return e.Container != "" && e.Container != api.ContainerKindNone
}

// cli is an isshoni command as the operator types it in this environment.
func cli(e api.DoctorEnv, args string) string {
	if inContainer(e) {
		return "docker compose exec isshoni isshoni " + args
	}
	return "sudo isshoni " + args
}

// restart is the command that restarts the server in this environment, or "" where doctor can't name it.
func restart(e api.DoctorEnv) string {
	switch {
	case inContainer(e):
		return "docker compose restart"
	case e.Systemd:
		return "sudo systemctl restart isshoni"
	}
	return ""
}

// owner is the service's user for a chown, by name where the report has one.
func owner(p params, e api.DoctorEnv) string {
	if inContainer(e) || e.User == "" || strings.HasPrefix(e.User, "uid ") {
		return strconv.FormatInt(p.int("uid"), 10)
	}
	return e.User
}

// publicIPText is the first line of the public_ip messages: the addresses and how they were found.
func publicIPText(p params) string {
	var parts []string
	if ip := p.str("ipv4"); ip != "" {
		how := ""
		switch p.str("ipv4Method") {
		case "config":
			how = "set in the config"
		case "interface":
			how = "on a network interface"
			if name := p.str("iface"); name != "" {
				how = "on interface " + name
			}
			if p.str("nat") == string(api.NATKindNone) {
				how += ", confirmed by STUN"
			}
		case "stun":
			how = "seen by STUN"
			if p.str("nat") == string(api.NATKindOneToOne) && p.has("local") {
				how += ", 1:1 NAT from " + p.str("local")
			}
		}
		if how != "" {
			ip += " (" + how + ")"
		}
		parts = append(parts, ip)
	}
	if ip := p.str("ipv6"); ip != "" {
		parts = append(parts, ip)
	}
	if len(parts) == 0 {
		return "no public address"
	}
	return strings.Join(parts, ", ")
}

// tlsSubject is "ip certificate for 203.0.113.7", "certificate for watch.example.com".
func tlsSubject(p params) string {
	kind := "certificate"
	switch p.str("mode") {
	case "ip":
		kind = "ip certificate"
	case "manual":
		kind = "manual certificate"
	}
	names := p.list("names")
	if len(names) == 0 && p.has("name") {
		names = []string{p.str("name")}
	}
	if len(names) == 0 {
		return kind
	}
	return kind + " for " + and(names)
}

// skewText is how far the clock is off: "1m12s ahead", "45s behind".
func skewText(p params) string {
	s := p.int("skewS")
	dir := "ahead"
	if s < 0 {
		s, dir = -s, "behind"
	}
	d := (time.Duration(s) * time.Second).String()
	return d + " " + dir
}

// ---- messages ----

var messagesEN = map[string]text{
	codeSkipNeedsServer: func(params, api.DoctorEnv) string { return "not checked: isshoni is not running" },
	codeSkipInterrupted: func(params, api.DoctorEnv) string { return "not checked: doctor was interrupted" },
	codeSkipUnsupported: func(p params, _ api.DoctorEnv) string { return "not checked: not available on " + p.str("os") },
	codeSkipNotReported: func(params, api.DoctorEnv) string { return "not checked: the server does not report it" },

	codeConfigOK: func(p params, _ api.DoctorEnv) string {
		switch {
		case p.bool("fileRead"):
			return p.str("file")
		case p.str("file") != "":
			return "no config file at " + p.str("file") + ": defaults, environment and flags"
		}
		return "defaults and flags"
	},
	codeConfigWarnings: func(p params, _ api.DoctorEnv) string {
		return plural(p.int("warnings"), "warning") + ": " + p.str("first")
	},
	codeConfigInvalid: func(p params, _ api.DoctorEnv) string {
		return plural(p.int("errors"), "error") + ": " + p.str("first")
	},

	codeDataDirOK: func(p params, _ api.DoctorEnv) string {
		detail := p.str("mode")
		if p.has("freeBytes") {
			detail += ", " + sizeText(p.int("freeBytes")) + " free"
		}
		return p.str("path") + " (" + detail + ")"
	},
	codeDataDirNotCreated: func(p params, _ api.DoctorEnv) string {
		return p.str("path") + " does not exist yet; isshoni creates it at its first start"
	},
	codeDataDirMissing: func(p params, _ api.DoctorEnv) string {
		return p.str("path") + " is gone while isshoni runs on it"
	},
	codeDataDirUnreadable: func(p params, _ api.DoctorEnv) string {
		return "can't look at " + p.str("path") + ": " + p.str("error")
	},
	codeDataDirNotDir: func(p params, _ api.DoctorEnv) string { return p.str("path") + " is not a directory" },
	codeDataDirNotMounted: func(p params, _ api.DoctorEnv) string {
		if p.has("error") {
			return "can't check that " + p.str("path") + " is a mounted volume: " + p.str("error")
		}
		return p.str("path") + " is not a mounted volume: your data would be lost when the container is removed"
	},
	codeDataDirEphemeralAllowed: func(p params, _ api.DoctorEnv) string {
		return p.str("path") + " is not a mounted volume, which ISSHONI_ALLOW_EPHEMERAL_DATA=1 allows: " +
			"the data is lost when the container is removed"
	},
	codeDataDirWrongOwner: func(p params, _ api.DoctorEnv) string {
		who := "isshoni runs"
		if p.bool("offline") {
			who = "doctor runs"
		}
		return fmt.Sprintf("%s belongs to uid %d, but %s as uid %d", p.str("path"), p.int("owner"), who, p.int("uid"))
	},
	codeDataDirNotWritable: func(p params, _ api.DoctorEnv) string {
		return p.str("path") + " is not writable for isshoni (" + p.str("error") + ")"
	},
	codeDataDirModeWide: func(p params, _ api.DoctorEnv) string {
		return p.str("path") + " has mode " + p.str("mode") + ", so other users can look into it; " +
			"isshoni makes it 0700 at its next start"
	},
	codeDataDirLowSpace: func(p params, _ api.DoctorEnv) string {
		return p.str("path") + " has only " + sizeText(p.int("freeBytes")) + " free"
	},

	codeSecretsOK: func(p params, _ api.DoctorEnv) string { return p.str("path") + " (" + p.str("mode") + ")" },
	codeSecretsNotCreated: func(p params, _ api.DoctorEnv) string {
		return p.str("path") + " does not exist yet; isshoni creates it at its first start"
	},
	codeSecretsMissing: func(p params, _ api.DoctorEnv) string {
		if p.bool("offline") {
			return p.str("path") + " is missing next to a database: the next start would make new keys and sign everyone out"
		}
		return p.str("path") + " is gone: the next start would make new keys and sign everyone out"
	},
	codeSecretsUnreadable: func(p params, _ api.DoctorEnv) string {
		return "can't read " + p.str("path") + ": " + p.str("error")
	},
	codeSecretsWrongOwner: func(p params, _ api.DoctorEnv) string {
		who := "isshoni runs"
		if p.bool("offline") {
			who = "doctor runs"
		}
		return fmt.Sprintf("%s belongs to uid %d, but %s as uid %d", p.str("path"), p.int("owner"), who, p.int("uid"))
	},
	codeSecretsCorrupt: func(p params, _ api.DoctorEnv) string {
		return p.str("path") + " can't be used (" + p.str("error") + ")\n" +
			"isshoni never replaces it, because new keys would sign everyone out"
	},
	codeSecretsModeWide: func(p params, _ api.DoctorEnv) string {
		return p.str("path") + " has mode " + p.str("mode") + "; isshoni makes it 0600 at its next start"
	},

	codeSchemaOK: func(p params, _ api.DoctorEnv) string {
		if p.has("path") {
			return fmt.Sprintf("schema %d (%s)", p.int("version"), p.str("path"))
		}
		return fmt.Sprintf("schema %d", p.int("version"))
	},
	codeSchemaNoDB: func(p params, _ api.DoctorEnv) string {
		return "no database yet at " + p.str("path") + "; isshoni creates it at its first start"
	},
	codeSchemaOlder: func(p params, _ api.DoctorEnv) string {
		return fmt.Sprintf("schema %d; this isshoni brings it to %d at its next start, after a backup",
			p.int("version"), p.int("binary"))
	},
	codeSchemaNewer: func(p params, _ api.DoctorEnv) string {
		return fmt.Sprintf("database schema %d is newer than this isshoni build supports (%d): isshoni refuses to start",
			p.int("version"), p.int("binary"))
	},
	codeSchemaHistoryMismatch: func(p params, _ api.DoctorEnv) string {
		return "the migration history in " + p.str("path") + " is not this isshoni build's: isshoni refuses to start"
	},
	codeSchemaCorrupt: func(p params, _ api.DoctorEnv) string {
		return p.str("path") + " failed its integrity check (" + p.str("error") + "): isshoni refuses to start"
	},
	codeSchemaUnreadable: func(p params, _ api.DoctorEnv) string {
		return "can't read " + p.str("path") + ": " + p.str("error")
	},

	codePublicIPOK: func(p params, _ api.DoctorEnv) string { return publicIPText(p) },
	codePublicIPNoIPv6: func(p params, e api.DoctorEnv) string {
		if inContainer(e) {
			return publicIPText(p) + "; IPv6 media off (Docker bridge)"
		}
		return publicIPText(p) + "; no public IPv6 address"
	},
	codePublicIPv6Only: func(p params, _ api.DoctorEnv) string {
		return publicIPText(p) + "; no public IPv4 address, so friends without IPv6 can't connect"
	},
	codePublicIPEphemeral: func(p params, _ api.DoctorEnv) string {
		return publicIPText(p) + "\n" +
			"The default public IPv4 changes on stop/start; in IP mode that moves the server's address and breaks " +
			"every link, installed app and push subscription"
	},
	codePublicIPPortForward: func(p params, _ api.DoctorEnv) string {
		if p.has("local") {
			return publicIPText(p) + "; this machine is " + p.str("local") + " behind a router"
		}
		return publicIPText(p) + "; this machine is behind a router"
	},
	codePublicIPSymmetric: func(p params, _ api.DoctorEnv) string {
		return publicIPText(p) + "; the NAT in front gives every connection another port (symmetric NAT), " +
			"so friends probably can't connect"
	},
	codePublicIPCGNAT: func(p params, _ api.DoctorEnv) string {
		return "this machine's address " + p.str("local") + " is carrier-grade NAT (100.64.0.0/10): " +
			"friends can't connect from outside"
	},
	codePublicIPNone: func(p params, _ api.DoctorEnv) string {
		first := "no public IP address found (no public address on an interface, no STUN answer)"
		switch {
		case p.bool("siteIsAddress") && p.bool("running"):
			return first + "\nisshoni is running but not ready, and nobody can open it"
		case p.bool("siteIsAddress"):
			return first + "\nisshoni will start, but it won't be ready"
		}
		return first + "\nthe site opens, but nobody can share or watch: media has no address to offer"
	},

	codeDNSOK: func(p params, _ api.DoctorEnv) string {
		out := p.str("domain") + " → " + and(append(p.list("a"), p.list("aaaa")...)) + " (this server)"
		if p.str("caa") == "allows" {
			out += "; CAA allows letsencrypt.org"
		}
		return out
	},
	codeDNSNoDomain: func(params, api.DoctorEnv) string { return "no domain: the site is this server's IP address" },
	codeDNSProxy: func(params, api.DoctorEnv) string {
		return "tls.mode is off: DNS points at the proxy in front, which doctor can't check"
	},
	codeDNSMissing: func(p params, _ api.DoctorEnv) string { return p.str("domain") + " doesn't resolve" },
	codeDNSWrong: func(p params, _ api.DoctorEnv) string {
		here := p.str("ip")
		if here == "" {
			here = p.str("ipv6")
		}
		return p.str("domain") + " points to " + and(p.list("others")) + ", but this server is " + here
	},
	codeDNSAlsoElsewhere: func(p params, _ api.DoctorEnv) string {
		return p.str("domain") + " points to this server and also to " + and(p.list("others")) +
			": visitors and Let's Encrypt reach either one"
	},
	codeDNSNoA: func(p params, _ api.DoctorEnv) string {
		return p.str("domain") + " has an AAAA record only: friends without IPv6 can't open it"
	},
	codeDNSAAAAWrong: func(p params, _ api.DoctorEnv) string {
		return p.str("domain") + " has the AAAA record " + and(p.list("others")) +
			", which is not this server: browsers that prefer IPv6 end up elsewhere"
	},
	codeDNSCAAForbids: func(p params, _ api.DoctorEnv) string {
		return "a CAA record on " + p.str("caaName") + " doesn't allow letsencrypt.org"
	},
	codeDNSLookupFailed: func(p params, _ api.DoctorEnv) string {
		return "could not look up " + p.str("domain") + ": " + p.str("error")
	},
	codeDNSUnverified: func(p params, _ api.DoctorEnv) string {
		return p.str("domain") + " → " + and(append(p.list("a"), p.list("aaaa")...)) +
			", but this server's public address is unknown (see public_ip)"
	},

	codeTLSOK: func(p params, _ api.DoctorEnv) string {
		out := tlsSubject(p)
		if p.has("notAfter") {
			out += ", valid until " + timeText(p.str("notAfter"))
		}
		if p.has("nextRenewal") {
			out += ", renews " + dateText(p.str("nextRenewal"))
		}
		return out
	},
	codeTLSOff: func(params, api.DoctorEnv) string { return "off: the proxy in front has the certificate" },
	codeTLSPending: func(p params, _ api.DoctorEnv) string {
		return "getting the " + tlsSubject(p) + "; this usually takes less than a minute"
	},
	codeTLSFailing: func(p params, _ api.DoctorEnv) string {
		return "no certificate yet: the last attempt to get the " + tlsSubject(p) + " failed; isshoni keeps trying"
	},
	codeTLSNoCert: func(p params, _ api.DoctorEnv) string {
		return fmt.Sprintf("still no %s, %d minutes after the start", tlsSubject(p), p.int("uptimeS")/60)
	},
	codeTLSNoPublicIP: func(params, api.DoctorEnv) string {
		return "no certificate: there is no public IP address to get one for (see public_ip)"
	},
	codeTLSRenewalFailing: func(p params, _ api.DoctorEnv) string {
		out := tlsSubject(p)
		if p.has("notAfter") {
			out += ", valid until " + timeText(p.str("notAfter"))
		}
		return out + ", but its renewal is failing"
	},
	codeTLSReloadFailed: func(p params, _ api.DoctorEnv) string {
		return "the new files in tls.cert_file and tls.key_file were refused; the " + tlsSubject(p) +
			" loaded before keeps serving"
	},
	codeTLSExpiresSoon: func(p params, _ api.DoctorEnv) string {
		return fmt.Sprintf("%s ends on %s, in %s", tlsSubject(p), dateText(p.str("notAfter")), plural(p.int("days"), "day"))
	},
	codeTLSExpired: func(p params, _ api.DoctorEnv) string {
		if p.has("notAfter") {
			return "the " + tlsSubject(p) + " expired on " + dateText(p.str("notAfter"))
		}
		return "the " + tlsSubject(p) + " is expired or not valid yet"
	},
	codeTLSNotLoaded: func(params, api.DoctorEnv) string {
		return "no certificate: tls.cert_file and tls.key_file could not be loaded"
	},

	codeClockOK: func(p params, _ api.DoctorEnv) string {
		var parts []string
		if p.str("sync") == "yes" {
			parts = append(parts, "synchronized")
		}
		if p.has("skewS") {
			parts = append(parts, "within "+plural(max(p.int("skewS"), -p.int("skewS"), 1), "second")+" of "+p.str("host"))
		}
		return strings.Join(parts, ", ")
	},
	codeClockUnknown: func(p params, _ api.DoctorEnv) string {
		if p.has("error") {
			return "can't tell: the kernel's sync state is not readable here, and " + p.str("host") +
				" did not answer (" + p.str("error") + ")"
		}
		return "can't tell: the kernel's sync state is not readable here"
	},
	codeClockUnsynced: func(params, api.DoctorEnv) string {
		return "the clock is not synchronized: no time service is keeping it right"
	},
	codeClockSkew: func(p params, _ api.DoctorEnv) string {
		return "the clock is " + skewText(p) + " (compared with " + p.str("host") + ")"
	},
	codeClockCertTime: func(p params, _ api.DoctorEnv) string {
		return "the clock (" + timeText(p.str("now")) + ") looks far off: the certificate of " + p.str("host") +
			" is not valid at that time"
	},

	codeUDPBuffersOK: func(p params, _ api.DoctorEnv) string {
		switch {
		case p.has("rmemMax") && p.has("rcvBuf"):
			return fmt.Sprintf("net.core.rmem_max is %d, the media sockets have %d", p.int("rmemMax"), p.int("rcvBuf"))
		case p.has("rmemMax"):
			return fmt.Sprintf("net.core.rmem_max is %d, net.core.wmem_max is %d", p.int("rmemMax"), p.int("wmemMax"))
		}
		return fmt.Sprintf("the media sockets have %d bytes to receive and %d to send", p.int("rcvBuf"), p.int("sndBuf"))
	},
	codeUDPBuffersLow: func(p params, _ api.DoctorEnv) string {
		want := p.int("want")
		switch {
		case p.has("rmemMax") && p.int("rmemMax") < want:
			return fmt.Sprintf("net.core.rmem_max is %d, isshoni wants %d", p.int("rmemMax"), want)
		case p.has("wmemMax") && p.int("wmemMax") < want:
			return fmt.Sprintf("net.core.wmem_max is %d, isshoni wants %d", p.int("wmemMax"), want)
		case p.int("rcvBuf") < want:
			return fmt.Sprintf("the media sockets receive into %d bytes, isshoni wants %d", p.int("rcvBuf"), want)
		}
		return fmt.Sprintf("the media sockets send from %d bytes, isshoni wants %d", p.int("sndBuf"), want)
	},
	codeUDPBuffersUDPOff: func(params, api.DoctorEnv) string { return "UDP media is turned off (listen.ice_udp is empty)" },
	codeUDPBuffersUnknown: func(params, api.DoctorEnv) string {
		return "not checked: net.core.rmem_max can't be read here, and isshoni is not running"
	},

	codePortsListening: func(p params, _ api.DoctorEnv) string {
		return "listening on " + strings.Join(p.list("listening"), ", ") + " (local check only)\n" + localOnlyNote
	},
	codePortsFree: func(p params, _ api.DoctorEnv) string {
		return portsFreeText(p) + " (local check only)\n" + localOnlyNote
	},
	codePortsDisabled: func(p params, _ api.DoctorEnv) string {
		first := portsFreeText(p)
		if p.has("listening") {
			first = "listening on " + strings.Join(p.list("listening"), ", ")
		}
		off := make([]string, 0, len(p.list("disabled")))
		keys := p.list("disabledKeys")
		for i, label := range p.list("disabled") {
			if i < len(keys) {
				label += " (" + keys[i] + " is empty)"
			}
			off = append(off, label)
		}
		return first + "; turned off: " + strings.Join(off, ", ") + " (local check only)\n" + localOnlyNote
	},
	codePortsNotListening: func(p params, _ api.DoctorEnv) string {
		out := "not listening on " + strings.Join(p.list("missing"), ", ")
		if p.has("listening") {
			out += "; listening on " + strings.Join(p.list("listening"), ", ")
		}
		return out + " (local check only)\n" + localOnlyNote
	},
	codePortsInUse: func(p params, _ api.DoctorEnv) string {
		ports := p.list("inUse")
		verb := "is"
		if len(ports) > 1 {
			verb = "are"
		}
		return and(ports) + " " + verb + " in use by another program (local check only)\n" + localOnlyNote
	},

	codeFirewallHintOpenPorts: func(p params, _ api.DoctorEnv) string {
		out := firewallText(api.CloudProvider(p.str("provider")), portsText(p))
		if p.bool("proxy") {
			out += "\nTCP 80 and 443 belong to the proxy in front: open them for it"
		}
		if p.bool("dockerBridge") {
			out += "\nDocker publishes ports past ufw: a published port is open even when ufw denies it"
		}
		return out
	},

	codeContainerNone: func(params, api.DoctorEnv) string { return "not in a container" },
	codeContainerDetected: func(p params, _ api.DoctorEnv) string {
		kind := map[string]string{"docker": "Docker", "podman": "Podman"}[p.str("kind")]
		if kind == "" {
			kind = "a container"
		}
		switch p.str("network") {
		case networkBridge:
			return kind + ", bridge network: publish " + portsText(p) + " under the same numbers (443:443, " +
				"7882:7882/udp), because isshoni tells browsers the container's ports"
		case networkHost:
			return kind + ", host network: isshoni uses the host's ports directly"
		}
		return kind + ": ports published from a bridge network must keep their numbers (443:443, 7882:7882/udp)"
	},

	codeLANPrivacyMacOS: func(params, api.DoctorEnv) string {
		return "macOS: a browser on a Mac needs the Local Network permission (System Settings → Privacy & " +
			"Security) to reach this server over the LAN; localhost is not affected"
	},
	codeLANPrivacyNotMacOS: func(params, api.DoctorEnv) string { return "not a macOS host" },

	codeTransferOK: func(p params, _ api.DoctorEnv) string {
		return fmt.Sprintf("%s of %d GB out in %s (%d %%)", sizeText(p.int("egressBytes")), p.int("alertGb"),
			p.str("month"), p.int("pct"))
	},
	codeTransferNoLimit: func(p params, _ api.DoctorEnv) string {
		return sizeText(p.int("egressBytes")) + " out in " + p.str("month") + "; no alert limit set"
	},
	codeTransferHigh: func(p params, _ api.DoctorEnv) string { return transferWarnText(p) },
	codeTransferOver: func(p params, _ api.DoctorEnv) string { return transferWarnText(p) },

	codeReleaseOK: func(p params, _ api.DoctorEnv) string {
		return p.str("version") + " is the latest release"
	},
	codeReleaseNotChecked: func(p params, _ api.DoctorEnv) string {
		return p.str("version") + "; no release check yet (it runs once a day, unless it is switched off)"
	},
	codeReleaseUpdate: func(p params, _ api.DoctorEnv) string {
		return "isshoni " + p.str("latest") + " is available (this is " + p.str("version") + ")"
	},
	codeReleaseSecurityUpdate: func(p params, _ api.DoctorEnv) string {
		return "isshoni " + p.str("latest") + " is available and fixes a security problem (this is " + p.str("version") + ")"
	},

	codeNofileOK: func(p params, _ api.DoctorEnv) string { return nofileText(p) },
	codeNofileLow: func(p params, _ api.DoctorEnv) string {
		return nofileText(p) + ", isshoni wants " + strconv.FormatInt(p.int("want"), 10)
	},

	codeBandwidthEstimate: func(p params, _ api.DoctorEnv) string { return bandwidthText(p) },
	codeBandwidthNearNIC: func(p params, _ api.DoctorEnv) string {
		return bandwidthText(p) + fmt.Sprintf("\nthat is more than 80 %% of %s's %d Mbps", p.str("iface"), p.int("nicMbps"))
	},
}

// localOnlyNote ends every ports message (04 §13.2).
const localOnlyNote = "This only checks this machine. The browser connection test checks from outside."

func portsFreeText(p params) string {
	free := p.list("free")
	out := "no port could be checked"
	switch len(free) {
	case 0:
	case 1:
		out = free[0] + " is free"
	default:
		out = and(free) + " are free"
	}
	if unchecked := p.list("unchecked"); len(unchecked) > 0 {
		why := "not checked"
		if p.bool("uncheckedDenied") {
			why = "not checked without root"
		}
		out += "; " + why + ": " + strings.Join(unchecked, ", ")
	}
	return out
}

func transferWarnText(p params) string {
	out := fmt.Sprintf("%s of %d GB out in %s (%d %%)", sizeText(p.int("egressBytes")), p.int("alertGb"),
		p.str("month"), p.int("pct"))
	if p.has("projectedEgressBytes") {
		out += "; at this rate the month ends at " + sizeText(p.int("projectedEgressBytes"))
	}
	return out
}

func nofileText(p params) string {
	out := "open-file limit " + strconv.FormatInt(p.int("limit"), 10)
	if p.bool("offline") {
		out += " (this shell's; the service gets its own from its unit or container)"
	}
	return out
}

func bandwidthText(p params) string {
	session := number(p.num("hours")) + "-hour session"
	// The whole gigabytes come from the wire rate, not from transferPerSessionGb, which is rounded to one place
	// already: rounding 99.49 GB twice would print 100.
	gb := p.num("egressWireMbps") * p.num("hours") * secondsPerHour / 8 / 1000
	return fmt.Sprintf("%d people, %d sharing: ~%s Mbps egress (~%.0f on the wire), ~%.0f GB per %s",
		p.int("people"), p.int("sharing"), strconv.FormatFloat(p.num("egressMediaMbps"), 'f', 1, 64),
		p.num("egressWireMbps"), gb, session)
}

// firewallText is the provider-specific line of firewall_hint (04 §13.3). 05's catalog has the same content per
// provider under fix.firewall.<provider>.
func firewallText(provider api.CloudProvider, ports string) string {
	allow := "allow " + ports
	switch provider {
	case api.CloudProviderAWS:
		return "AWS: EC2 console → Security Groups → inbound rules of this instance's group: " + allow
	case api.CloudProviderGCP:
		return "Google Cloud: VPC network → Firewall → add a rule for incoming traffic: " + allow
	case api.CloudProviderAzure:
		return "Azure: this VM's network security group → inbound security rules: " + allow
	case api.CloudProviderOracle:
		return "Oracle Cloud: the subnet's security list → ingress rules: " + allow +
			"\nOracle's images also block these ports with their own iptables rules: open them on the server too"
	case api.CloudProviderHetzner:
		return "Hetzner: Cloud Console → Firewalls → " + allow
	case api.CloudProviderDigitalOcean:
		return "DigitalOcean: Networking → Firewalls → the firewall of this Droplet: " + allow
	case api.CloudProviderVultr:
		return "Vultr: Firewall → the group linked to this instance: " + allow
	case api.CloudProviderLinode:
		return "Akamai (Linode): Cloud Manager → Firewalls → the firewall of this Linode: " + allow
	case api.CloudProviderScaleway:
		return "Scaleway: this instance's security group → inbound rules: " + allow
	case api.CloudProviderOVH:
		return "OVHcloud: this instance's security group → inbound rules: " + allow
	case api.CloudProviderAlibaba:
		return "Alibaba Cloud: this instance's security group → inbound rules: " + allow
	case api.CloudProviderTencent:
		return "Tencent Cloud: this instance's security group → inbound rules: " + allow
	}
	return "open " + ports + " in your provider's firewall or security group, and in this machine's own firewall if it has one"
}

// ---- fixes ----

var fixesEN = map[string]text{
	fixConfigCheck: func(p params, e api.DoctorEnv) string {
		check := "`" + strings.TrimPrefix(cli(e, "config check"), "sudo ") + "` lists every problem with its fix"
		if fix := p.str("firstFix"); fix != "" {
			if p.int("errors")+p.int("warnings") > 1 {
				return fix + "\n" + check
			}
			return fix
		}
		return check
	},

	fixDataDirSudo:  func(params, api.DoctorEnv) string { return "run doctor as root: sudo isshoni doctor" },
	fixSecretsSudo:  func(params, api.DoctorEnv) string { return "run doctor as root: sudo isshoni doctor" },
	fixSchemaSudo:   func(params, api.DoctorEnv) string { return "run doctor as root: sudo isshoni doctor" },
	fixDataDirOwner: func(p params, e api.DoctorEnv) string { return ownerFix(p, e, true) },
	fixSecretsOwner: func(p params, e api.DoctorEnv) string { return ownerFix(p, e, false) },
	fixDataDirNotDir: func(params, api.DoctorEnv) string {
		return "move that file away, or set data_dir to a directory"
	},
	fixDataDirMount: func(p params, _ api.DoctorEnv) string {
		return "Mount a volume at " + p.str("path") + " (see compose.yaml), e.g. -v isshoni-data:" + p.str("path")
	},
	fixDataDirMode:  func(p params, _ api.DoctorEnv) string { return "sudo chmod 700 " + shellQuote(p.str("path")) },
	fixSecretsMode:  func(p params, _ api.DoctorEnv) string { return "sudo chmod 600 " + shellQuote(p.str("path")) },
	fixDataDirSpace: func(params, api.DoctorEnv) string { return "free some space, or move data_dir to a larger disk" },

	fixSecretsRestore: func(_ params, e api.DoctorEnv) string {
		if inContainer(e) {
			return "restore it from a backup: docker compose stop && docker compose run --rm isshoni admin restore " +
				"--offline <backup> && docker compose up -d"
		}
		return "restore it from a backup: sudo systemctl stop isshoni && sudo -u isshoni isshoni admin restore " +
			"--offline <backup> && sudo systemctl start isshoni"
	},

	fixSchemaRestore: func(p params, e api.DoctorEnv) string {
		cmd := RestoreCommand(p.str("backup"), inContainer(e))
		if msg := p.str("storeMessage"); msg != "" {
			return msg + "\n" + cmd
		}
		return schemaNewerText(p) + "\n" + cmd
	},
	fixSchemaUpgrade: func(p params, _ api.DoctorEnv) string {
		if msg := p.str("storeMessage"); msg != "" {
			return msg
		}
		return schemaNewerText(p)
	},
	fixSchemaSameBuild: func(p params, _ api.DoctorEnv) string {
		if v := p.str("lastAppVersion"); v != "" {
			return "install isshoni " + v + ", which used this database last, or restore a backup with `isshoni admin restore --offline <backup>`"
		}
		return "install the isshoni version that used this database last, or restore a backup with `isshoni admin restore --offline <backup>`"
	},
	fixSchemaRestoreBackup: func(_ params, e api.DoctorEnv) string {
		if inContainer(e) {
			return "restore a backup: docker compose run --rm isshoni admin restore --offline <backup>"
		}
		return "restore a backup: sudo -u isshoni isshoni admin restore --offline <backup>"
	},

	fixPublicIPSet: func(_ params, e api.DoctorEnv) string {
		if inContainer(e) {
			return "set ISSHONI_PUBLIC_IP in .env to this server's public address, or set a domain, then run docker compose up -d\n" +
				"(if the network wasn't up yet when isshoni started: docker compose restart)"
		}
		out := "set public_ip to this server's public address, or set domain, then restart isshoni"
		if cmd := restart(e); cmd != "" {
			return out + "\n(if the network wasn't up yet when isshoni started: " + cmd + ")"
		}
		return out + "\n(if the network wasn't up yet when isshoni started, restarting is enough)"
	},
	fixPublicIPNAT: func(params, api.DoctorEnv) string {
		return "run isshoni where it has a public IPv4 address of its own, for example on a small VPS, or ask your " +
			"internet provider for a public address"
	},
	fixPublicIPForward: func(p params, _ api.DoctorEnv) string {
		if p.has("local") {
			return "on your router, forward " + portsText(p) + " to " + p.str("local")
		}
		return "on your router, forward " + portsText(p) + " to this machine"
	},
	fixPublicIPStatic: func(p params, _ api.DoctorEnv) string {
		if p.str("provider") == string(api.CloudProviderGCP) {
			return "Reserve a static external IP (GCP) for this instance, or use a domain"
		}
		return "Attach an Elastic IP (AWS) to this instance, or use a domain"
	},

	fixDNSAddRecord: func(p params, _ api.DoctorEnv) string {
		if ip := p.str("ip"); ip != "" {
			return "add an A record for " + p.str("domain") + " pointing to " + ip
		}
		return "add an A record for " + p.str("domain") + " pointing to this server"
	},
	fixDNSPointHere: func(p params, _ api.DoctorEnv) string {
		switch {
		case p.has("ip"):
			return "point the A record of " + p.str("domain") + " to " + p.str("ip") + " only, then run doctor again " +
				"(a changed record can take a while to show)"
		case p.has("ipv6"):
			return "point the AAAA record of " + p.str("domain") + " to " + p.str("ipv6")
		}
		return "point " + p.str("domain") + " to this server"
	},
	fixDNSAAAA: func(p params, _ api.DoctorEnv) string {
		if p.has("ipv6") {
			return "point the AAAA record of " + p.str("domain") + " to " + p.str("ipv6") + ", or remove it"
		}
		return "remove the AAAA record of " + p.str("domain") + ": this server has no public IPv6 address"
	},
	fixDNSCAA: func(p params, _ api.DoctorEnv) string {
		return "add the CAA record `0 issue \"letsencrypt.org\"` to " + p.str("caaName") + ", or remove its CAA records"
	},

	// The fix texts of 04 §8.7, from what the server's status says: the certificate's name, the domain and this
	// server's address. The server's log has the CA's own words for each failed attempt.
	tlsACMEUnreachable: func(p params, _ api.DoctorEnv) string {
		return "Let's Encrypt couldn't connect to " + p.str("name") + " on port 80 or 443. Open TCP 80 and 443 in your cloud firewall"
	},
	tlsDNSMissing: func(p params, _ api.DoctorEnv) string {
		if p.has("ip") {
			return p.str("name") + " doesn't resolve. Add an A record pointing to " + p.str("ip")
		}
		return p.str("name") + " doesn't resolve. Add an A record pointing to this server"
	},
	tlsDNSWrong: func(p params, _ api.DoctorEnv) string {
		if p.has("ip") {
			return p.str("name") + " doesn't point to this server (" + p.str("ip") + "). Change its A record (see dns)"
		}
		return p.str("name") + " doesn't point to this server. Change its A record (see dns)"
	},
	tlsRateLimited: func(_ params, e api.DoctorEnv) string {
		return "Let's Encrypt rate limit reached; " + logHint(e) + " says until when. Restore certmagic/ from a backup if you reinstalled"
	},
	tlsIPRejected: func(p params, _ api.DoctorEnv) string {
		return "Let's Encrypt refused " + p.str("name") + " (not a public address?)"
	},
	tlsCAAForbids: func(p params, _ api.DoctorEnv) string {
		return "A CAA record on " + p.str("name") + " doesn't allow letsencrypt.org"
	},
	tlsACMEFailed: func(_ params, e api.DoctorEnv) string {
		return "the certificate authority refused or could not be reached; " + logHint(e) + " has its answer"
	},
	tlsCertUnreadable: func(params, api.DoctorEnv) string {
		return "isshoni can't read tls.cert_file or tls.key_file. Check the paths, and let the user that isshoni runs " +
			"as read both, for example: sudo chgrp isshoni <file> && sudo chmod 640 <file>"
	},
	tlsCertInvalid: func(params, api.DoctorEnv) string {
		return "tls.cert_file and tls.key_file are not a certificate with its private key. Put the full chain in " +
			"tls.cert_file and the matching key in tls.key_file, both as PEM"
	},
	tlsCertExpired: func(params, api.DoctorEnv) string {
		return "The certificate in tls.cert_file is expired or not valid yet. Replace it and its key; isshoni loads " +
			"new files within a minute"
	},
	fixTLSReplace: func(params, api.DoctorEnv) string {
		return "replace the files in tls.cert_file and tls.key_file; isshoni loads new files within a minute"
	},

	fixClockNTP: func(_ params, e api.DoctorEnv) string {
		if inContainer(e) {
			return "on the Docker host, turn on time synchronization: sudo timedatectl set-ntp true"
		}
		return "turn on time synchronization: sudo timedatectl set-ntp true"
	},

	fixUDPBuffersSysctl: func(p params, e api.DoctorEnv) string {
		want := strconv.FormatInt(p.int("want"), 10)
		cmd := `printf 'net.core.rmem_max=` + want + `\nnet.core.wmem_max=` + want +
			`\n' | sudo tee /etc/sysctl.d/60-isshoni.conf && sudo sysctl --system`
		if inContainer(e) {
			return "on the Docker host: " + cmd
		}
		return cmd
	},
	fixUDPBuffersSysctlReload: func(params, api.DoctorEnv) string { return "sudo sysctl --system" },
	fixUDPBuffersRestart: func(_ params, e api.DoctorEnv) string {
		if cmd := restart(e); cmd != "" {
			return "restart isshoni so that its sockets get the larger buffers: " + cmd
		}
		return "restart isshoni so that its sockets get the larger buffers"
	},

	fixPortsFree: func(p params, _ api.DoctorEnv) string {
		var web, media []string
		for _, key := range p.list("inUseKeys") {
			if key == "listen.https" || key == "listen.http" {
				web = append(web, key)
			} else {
				media = append(media, key)
			}
		}
		var lines []string
		if len(web) > 0 {
			lines = append(lines, `stop the other program (another web server?), or run isshoni behind it with tls.mode = "off"`)
		}
		if len(media) > 0 {
			lines = append(lines, "stop the other program (another isshoni?), or change "+and(media))
		}
		return strings.Join(lines, "\n")
	},

	fixTransferReview: func(params, api.DoctorEnv) string {
		return "compare with your provider's allowance; the limit is Admin → Settings → transfer alert " +
			"(limits.transfer_alert_gb), and lower share bitrates use less"
	},

	fixReleaseUpgrade: func(p params, e api.DoctorEnv) string {
		out := "run the installer again to upgrade"
		if inContainer(e) {
			out = "docker compose pull && docker compose up -d"
		}
		if url := p.str("url"); url != "" {
			out += "\n" + url
		}
		return out
	},

	fixNofileLimit: func(_ params, e api.DoctorEnv) string {
		if inContainer(e) {
			return "raise the container's limit, for example in compose.yaml: ulimits: {nofile: 65536}"
		}
		return "raise LimitNOFILE in isshoni's systemd unit (sudo systemctl edit isshoni), then restart it"
	},

	fixBandwidthLink: func(params, api.DoctorEnv) string {
		return "plan for a faster uplink, fewer people at once, or a lower limit on share bitrates " +
			"(limits.max_bitrate_kbps)"
	},
}

// ownerFix gives a data file or directory to the service's user (04 §5.1): by name on a systemd host, by number on
// a container's host.
func ownerFix(p params, e api.DoctorEnv, recursive bool) string {
	r := ""
	if recursive {
		r = "-R "
	}
	if !p.has("uid") {
		return "let the user that isshoni runs as read and write " + p.str("path")
	}
	if inContainer(e) {
		return fmt.Sprintf("on the container's host, run: sudo chown %s%d:%d <the host path of %s>",
			r, p.int("uid"), p.int("uid"), p.str("path"))
	}
	out := "sudo chown " + r + owner(p, e) + " " + shellQuote(p.str("path"))
	if p.bool("offline") && p.has("owner") {
		out += fmt.Sprintf("\n(or run doctor as the owner: sudo -u '#%d' isshoni doctor)", p.int("owner"))
	}
	return out
}

// logHint names where the server's log is in this environment.
func logHint(e api.DoctorEnv) string {
	if inContainer(e) {
		return "the server's log (docker compose logs isshoni)"
	}
	return "the server's log (journalctl -u isshoni)"
}

// schemaNewerText says in doctor's words what 03's message for a newer schema says, for a caller that did not
// pass that message along.
func schemaNewerText(p params) string {
	v := p.str("lastAppVersion")
	out := "the database was last used by a newer isshoni. Install a newer isshoni"
	if v != "" {
		out = "the database was last used by isshoni " + v + ". Install isshoni " + v + " or newer"
	}
	if p.has("backup") {
		return out + ", or restore the database from before the upgrade: " + p.str("backup") +
			" (changes made after that backup are lost)"
	}
	return out + "; no backup from before the upgrade was found"
}
