package doctor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/netx"
	"github.com/MoonWX/isshoni/internal/version"
)

// The checks of how the server reaches the network and the network reaches it: public_ip, dns, tls, clock, ports
// and firewall_hint (04 §13.2).

// publicState is what a run knows about the server's public addresses: the running server's own detection
// (04 §7.4), or offline the one doctor runs itself.
type publicState struct {
	v4, v6             netip.Addr
	v4Method, v6Method string
	local              netip.Addr // the IPv4 of the default route's interface
	nat                api.NATKind
	err                string // why the offline detection failed, if it did
}

// publicAddrs returns the public addresses, found once per run. With a running server they are the ones the server
// started with: its site, its certificate's name and its ICE candidates are built on them, so those are the
// addresses to judge, and no STUN server is asked again.
func (r *run) publicAddrs(ctx context.Context) publicState {
	r.publicOnce.Do(func() {
		st := publicState{nat: api.NATKindUnknown}
		parse := func(s string) netip.Addr {
			a, _ := netip.ParseAddr(s)
			return a
		}
		if r.live != nil {
			st.v4, st.v4Method = parse(r.live.PublicIPv4), r.live.PublicIPv4Method
			st.v6, st.v6Method = parse(r.live.PublicIPv6), r.live.PublicIPv6Method
			st.local = parse(r.live.LocalIPv4)
			if r.live.NAT != "" {
				st.nat = r.live.NAT
			}
			r.public = st
			return
		}
		res, err := netx.DetectPublicAddrs(ctx, netx.DetectOptions{
			PublicIP:      r.cfg.PublicIP,
			PublicIPv6:    r.cfg.PublicIPv6,
			STUNServers:   r.cfg.Network.STUNServers,
			IPv6:          r.cfg.Network.IPv6,
			STUN:          r.env.STUN,
			Interfaces:    r.env.Interfaces,
			CloudProvider: r.host.Provider,
			InContainer:   r.inContainer(),
		})
		st.v4, st.v4Method = res.V4, string(res.V4Method)
		st.v6, st.v6Method = res.V6, string(res.V6Method)
		st.local, st.nat = res.LocalV4, res.NAT
		if err != nil {
			st.err = err.Error()
		}
		r.public = st
	})
	return r.public
}

// siteIsAddress reports whether the site is the server's public address: ip mode, and manual mode without a domain
// (04 §4.4). Such a server needs the address to have a site at all (04 §6.2).
func (r *run) siteIsAddress() bool {
	mode := r.tlsMode()
	return mode == config.TLSIP || (mode == config.TLSManual && r.cfg.Domain == "")
}

// checkPublicIP reports the public addresses, how they were found and what kind of NAT is in front (04 §7.4).
func checkPublicIP(ctx context.Context, r *run) result {
	pub := r.publicAddrs(ctx)
	p := params{"nat": string(pub.nat)}
	if pub.v4.IsValid() {
		p["ipv4"], p["ipv4Method"] = pub.v4.String(), pub.v4Method
		if name := r.ifaceOf(pub.v4); name != "" {
			p["iface"] = name
		}
	}
	if pub.v6.IsValid() {
		p["ipv6"], p["ipv6Method"] = pub.v6.String(), pub.v6Method
	}
	if pub.local.IsValid() {
		p["local"] = pub.local.String()
	}
	provider := r.host.Provider
	switch {
	case !pub.v4.IsValid() && !pub.v6.IsValid():
		// No address at all (04 §6.2, §13.2): the server runs and is not ready where its site is its address, and
		// media has no address to advertise in any mode.
		p["siteIsAddress"], p["running"] = r.siteIsAddress(), !r.offline()
		if pub.err != "" {
			p["error"] = pub.err
		}
		return failResult(codePublicIPNone, p, fixPublicIPSet)
	case pub.nat == api.NATKindSymmetric:
		return failResult(codePublicIPSymmetric, p, fixPublicIPNAT)
	case pub.nat == api.NATKindCGNATLikely:
		return failResult(codePublicIPCGNAT, p, fixPublicIPNAT)
	case !pub.v4.IsValid():
		return warnResult(codePublicIPv6Only, p, "")
	case pub.nat == api.NATKindPortForward:
		p["tcpPorts"], p["udpPorts"] = r.publicPorts()
		return warnResult(codePublicIPPortForward, p, fixPublicIPForward)
	case r.tlsMode() == config.TLSIP && (provider == api.CloudProviderAWS || provider == api.CloudProviderGCP):
		// These two clouds give an instance a new public IPv4 on every stop and start unless a static one is
		// attached. Where the site is the address, that breaks everything that remembers it.
		p["provider"] = string(provider)
		return infoResult(codePublicIPEphemeral, p).withFix(fixPublicIPStatic)
	case !pub.v6.IsValid() && r.cfg.Network.IPv6 && r.cfg.PublicIPv6 != "off":
		return infoResult(codePublicIPNoIPv6, p)
	}
	return okResult(codePublicIPOK, p)
}

// dnsTimeout bounds the dns check's lookups.
const dnsTimeout = 5 * time.Second

// letsEncryptIssuer is the CAA issuer domain of Let's Encrypt.
const letsEncryptIssuer = "letsencrypt.org"

// checkDNS compares the domain's A and AAAA records with the server's public addresses, and in auto mode looks
// for a CAA record that keeps Let's Encrypt out. It applies where the server answers for a domain with its own
// certificate (auto, and manual with a domain); behind a proxy (off) DNS points at the proxy.
func checkDNS(ctx context.Context, r *run) result {
	mode := r.tlsMode()
	domain := strings.ToLower(strings.TrimSuffix(r.cfg.Domain, "."))
	switch {
	case mode == config.TLSOff:
		return okResult(codeDNSProxy, nil)
	case domain == "" || mode == config.TLSIP:
		return okResult(codeDNSNoDomain, nil)
	}
	p := params{"domain": domain}
	pub := r.publicAddrs(ctx)
	if pub.v4.IsValid() {
		p["ip"] = pub.v4.String()
	}
	if pub.v6.IsValid() {
		p["ipv6"] = pub.v6.String()
	}

	lctx, cancel := context.WithTimeout(ctx, dnsTimeout)
	defer cancel()
	addrs, err := r.env.Resolver.LookupNetIP(lctx, "ip", domain)
	if err != nil {
		var de *net.DNSError
		if errors.As(err, &de) && de.IsNotFound {
			return failResult(codeDNSMissing, p, fixDNSAddRecord)
		}
		p["error"] = dnsErrText(err)
		return warnResult(codeDNSLookupFailed, p, "")
	}
	var a, aaaa []string
	var aOurs, aaaaOurs bool
	var aOthers, aaaaOthers []string
	for _, addr := range addrs {
		addr = addr.Unmap().WithZone("")
		if addr.Is4() {
			a = append(a, addr.String())
			if pub.v4.IsValid() && addr == pub.v4.Unmap() {
				aOurs = true
			} else {
				aOthers = append(aOthers, addr.String())
			}
			continue
		}
		aaaa = append(aaaa, addr.String())
		if pub.v6.IsValid() && addr == pub.v6.WithZone("") {
			aaaaOurs = true
		} else {
			aaaaOthers = append(aaaaOthers, addr.String())
		}
	}
	slices.Sort(a)
	slices.Sort(aaaa)
	if len(a) > 0 {
		p["a"] = a
	}
	if len(aaaa) > 0 {
		p["aaaa"] = aaaa
	}
	switch {
	case len(a) == 0 && len(aaaa) == 0:
		return failResult(codeDNSMissing, p, fixDNSAddRecord)
	case !pub.v4.IsValid() && !pub.v6.IsValid():
		// Nothing to compare with: public_ip says why.
		return warnResult(codeDNSUnverified, p, "")
	case len(a) > 0 && !aOurs:
		p["others"] = aOthers
		return failResult(codeDNSWrong, p, fixDNSPointHere)
	case len(a) == 0 && !aaaaOurs:
		p["others"] = aaaaOthers
		return failResult(codeDNSWrong, p, fixDNSPointHere)
	}

	// The records lead here. What is left are the ways they can still fail someone.
	if mode == config.TLSAuto && r.letsEncrypt() {
		caaName, forbids, checked := r.caaForbids(ctx, domain)
		switch {
		case forbids:
			p["caaName"] = caaName
			return failResult(codeDNSCAAForbids, p, fixDNSCAA)
		case !checked:
			p["caa"] = "unchecked"
		case caaName != "":
			p["caa"] = "allows"
		default:
			p["caa"] = "none"
		}
	}
	switch {
	case len(aOthers) > 0:
		p["others"] = aOthers
		return warnResult(codeDNSAlsoElsewhere, p, fixDNSPointHere)
	case len(aaaa) > 0 && !aaaaOurs, len(aaaaOthers) > 0:
		p["others"] = aaaaOthers
		return warnResult(codeDNSAAAAWrong, p, fixDNSAAAA)
	case len(a) == 0 && pub.v4.IsValid():
		return warnResult(codeDNSNoA, p, fixDNSAddRecord)
	}
	return okResult(codeDNSOK, p)
}

// dnsErrText is a resolver error on one short line.
func dnsErrText(err error) string {
	var de *net.DNSError
	if errors.As(err, &de) {
		switch {
		case de.IsTimeout:
			return "the resolver did not answer in time"
		case de.Err != "":
			return de.Err
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "the resolver did not answer in time"
	}
	return err.Error()
}

// The ACME directories of Let's Encrypt. tlsmgr takes them from certmagic; a test checks that these are the same.
const (
	letsEncryptProduction = "https://acme-v02.api.letsencrypt.org/directory"
	letsEncryptStaging    = "https://acme-staging-v02.api.letsencrypt.org/directory"
)

// acmeDirectory is the ACME directory the server orders its certificate from: tls.acme_ca, or Let's Encrypt,
// whose staging directory tls.acme_staging selects (as tlsmgr does).
func (r *run) acmeDirectory() string {
	ca := r.cfg.TLS.ACMECA
	if ca == "" {
		ca = letsEncryptProduction
	}
	if r.cfg.TLS.ACMEStaging && ca == letsEncryptProduction {
		ca = letsEncryptStaging
	}
	return ca
}

// letsEncrypt reports whether the certificate comes from Let's Encrypt, the CA the CAA check knows the name of.
func (r *run) letsEncrypt() bool {
	ca := r.acmeDirectory()
	return ca == letsEncryptProduction || ca == letsEncryptStaging
}

// caaForbids looks up the CAA records that apply to domain. forbids is true when they keep Let's Encrypt out;
// name is then the name that holds them. checked is false when the lookup failed: CAA is then left to the CA,
// which reports it as tls.caa_forbids if it matters.
func (r *run) caaForbids(ctx context.Context, domain string) (name string, forbids, checked bool) {
	cctx, cancel := context.WithTimeout(ctx, dnsTimeout)
	defer cancel()
	recs, err := r.env.CAA(cctx, domain)
	if err != nil {
		return "", false, false
	}
	if len(recs) == 0 {
		return "", false, true
	}
	return recs[0].Name, !caaAllows(recs, letsEncryptIssuer), true
}

// Durations of the tls check (04 §13.2).
const (
	tlsGrace         = 10 * time.Minute    // a server without a certificate fails after this long
	manualExpiryWarn = 14 * 24 * time.Hour // a manual certificate warns this long before it ends
)

// checkTLS reports the certificate: mode, names, issuer, end date, next renewal, and the last error's code
// (04 §8.7). Only a running server knows them.
func checkTLS(ctx context.Context, r *run) result {
	if r.offline() {
		return skipResult(codeSkipNeedsServer, nil)
	}
	st := r.live.TLS
	mode := config.TLSMode(st.Mode)
	if mode == "" {
		mode = r.tlsMode()
	}
	if mode == config.TLSOff {
		return okResult(codeTLSOff, nil)
	}
	pub := r.publicAddrs(ctx)
	p := params{"mode": string(mode)}
	names := slices.Clone(st.Names)
	if len(names) > 0 {
		p["names"] = names
	}
	// What the fix texts of 04 §8.7 name: the certificate's subject, the domain and this server's address.
	name := r.cfg.Domain
	if len(names) > 0 {
		name = names[0]
	}
	if r.cfg.Domain != "" {
		p["domain"] = strings.ToLower(r.cfg.Domain)
	}
	switch {
	case pub.v4.IsValid():
		p["ip"] = pub.v4.String()
	case pub.v6.IsValid():
		p["ip"] = pub.v6.String()
	}
	if name == "" {
		name, _ = p["ip"].(string)
	}
	if name != "" {
		p["name"] = name
	}
	if st.Issuer != "" {
		p["issuer"] = st.Issuer
	}
	if !st.NotAfter.IsZero() {
		p["notAfter"] = api.WireTime(st.NotAfter).String()
	}
	if !st.NextRenewal.IsZero() {
		p["nextRenewal"] = api.WireTime(st.NextRenewal).String()
	}
	now := r.now()
	expired := !st.NotAfter.IsZero() && !now.Before(st.NotAfter)
	errCode := st.LastErrorCode

	if st.Ready && !expired {
		switch {
		case errCode != "" && mode == config.TLSManual:
			// New files that could not be loaded: the pair loaded before keeps serving (04 §8.4).
			return warnResult(codeTLSReloadFailed, p, errCode)
		case errCode != "":
			return warnResult(codeTLSRenewalFailing, p, errCode)
		case mode == config.TLSManual && !st.NotAfter.IsZero() && st.NotAfter.Sub(now) < manualExpiryWarn:
			p["days"] = int(st.NotAfter.Sub(now) / (24 * time.Hour))
			return warnResult(codeTLSExpiresSoon, p, fixTLSReplace)
		}
		return okResult(codeTLSOK, p)
	}

	uptime := time.Duration(r.live.UptimeS) * time.Second
	switch {
	case expired || errCode == tlsCertExpired:
		if mode == config.TLSManual {
			return failResult(codeTLSExpired, p, tlsCertExpired)
		}
		return failResult(codeTLSExpired, p, errCode)
	case mode == config.TLSManual:
		// The files are not a certificate with its key, or can't be read (04 §8.4).
		return failResult(codeTLSNotLoaded, p, errCode)
	case mode == config.TLSIP && !pub.v4.IsValid() && !pub.v6.IsValid():
		// No ACME request was made, so there is no ACME error to name, and nothing is under way that ten minutes
		// could finish (04 §13.2).
		return failResult(codeTLSNoPublicIP, p, "")
	case uptime >= tlsGrace:
		p["uptimeS"] = r.live.UptimeS
		return failResult(codeTLSNoCert, p, errCode)
	case errCode != "":
		// From the first failed attempt on: install.sh's doctor output at its timeout names the cause.
		return warnResult(codeTLSFailing, p, errCode)
	}
	return infoResult(codeTLSPending, p)
}

// The codes of tlsmgr.Status.LastErrorCode (04 §8.7), which the tls check passes on as fix codes. doctor may not
// import tlsmgr (04 §2); a test checks that the two lists agree.
const (
	tlsACMEUnreachable = "tls.acme_unreachable"
	tlsDNSMissing      = "tls.dns_missing"
	tlsDNSWrong        = "tls.dns_wrong"
	tlsRateLimited     = "tls.rate_limited"
	tlsIPRejected      = "tls.ip_rejected"
	tlsCAAForbids      = "tls.caa_forbids"
	tlsACMEFailed      = "tls.acme_failed"
	tlsCertUnreadable  = "tls.cert_unreadable"
	tlsCertInvalid     = "tls.cert_invalid"
	tlsCertExpired     = "tls.cert_expired"
)

// Limits of the clock check (04 §13.2).
const (
	clockTimeout  = 5 * time.Second
	clockWarnSkew = 30 * time.Second
	clockFailSkew = 5 * time.Minute
)

// checkClock asks two sources whether the clock is right. The kernel's sync flag (Linux adjtimex, no network) says
// whether a time daemon steers the clock; where the call is filtered its state is unknown, which is no warning. In
// auto and ip mode, where a certificate order fails on a wrong clock, the Date header of the ACME directory gives
// the skew itself: one GET of a server the certificate needs anyway (04 §16).
func checkClock(ctx context.Context, r *run) result {
	p := params{}
	sync := "unknown"
	if r.env.Adjtimex != nil {
		if unsynced, err := r.env.Adjtimex(); err == nil {
			sync = "yes"
			if unsynced {
				sync = "no"
			}
		}
	}
	p["sync"] = sync

	skewKnown := false
	var skew time.Duration
	if mode := r.tlsMode(); mode == config.TLSAuto || mode == config.TLSIP {
		dir := r.acmeDirectory()
		if u, err := url.Parse(dir); err == nil {
			p["host"] = u.Host
		}
		var err error
		skew, err = r.clockSkew(ctx, dir)
		var certErr x509.CertificateInvalidError
		switch {
		case err == nil:
			skewKnown = true
			p["skewS"] = int64(skew.Round(time.Second) / time.Second)
		case errors.As(err, &certErr) && certErr.Reason == x509.Expired:
			// The CA's own certificate looks expired or not yet valid from here: the clock is far off, further
			// than a Date header could ever be read.
			p["now"] = api.WireTime(r.now()).String()
			return failResult(codeClockCertTime, p, fixClockNTP)
		default:
			p["error"] = httpErrText(err)
		}
	}
	abs := skew.Abs()
	switch {
	case skewKnown && abs > clockFailSkew:
		return failResult(codeClockSkew, p, fixClockNTP)
	case skewKnown && abs >= clockWarnSkew:
		return warnResult(codeClockSkew, p, fixClockNTP)
	case sync == "no":
		return warnResult(codeClockUnsynced, p, fixClockNTP)
	case skewKnown || sync == "yes":
		return okResult(codeClockOK, p)
	}
	return infoResult(codeClockUnknown, p)
}

// clockSkew returns how far the local clock is ahead of the Date header that the ACME directory at dir answers with
// (negative: behind). The header drops the fraction of its second, and the answer took some time to arrive, so the
// result is good to about a second, which is plenty for limits of 30 s and 5 min.
func (r *run) clockSkew(ctx context.Context, dir string) (time.Duration, error) {
	client, err := r.httpClient()
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, clockTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dir, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", version.UserAgent())
	sent := r.now()
	res, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	received := r.now()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	_ = res.Body.Close()
	date, err := http.ParseTime(res.Header.Get("Date"))
	if err != nil {
		return 0, fmt.Errorf("the answer has no usable Date header: %w", err)
	}
	// The server wrote the header somewhere between the two instants; the middle is the best guess.
	return sent.Add(received.Sub(sent) / 2).Sub(date), nil
}

// httpClient returns the client of the clock check: Env.HTTP, or one that sends nothing but the request, keeps no
// connection, and trusts tls.acme_ca_root when the ACME server is a private one.
func (r *run) httpClient() (*http.Client, error) {
	if r.env.HTTP != nil {
		return r.env.HTTP, nil
	}
	tr := &http.Transport{Proxy: http.ProxyFromEnvironment, DisableKeepAlives: true}
	if file := r.cfg.TLS.ACMECARoot; file != "" {
		pem, err := os.ReadFile(file) //nolint:gosec // G304: the operator's own path, from tls.acme_ca_root
		if err != nil {
			return nil, fmt.Errorf("tls.acme_ca_root: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("tls.acme_ca_root = %q holds no PEM certificate", file)
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Transport: tr, Timeout: clockTimeout}, nil
}

// httpErrText is the cause of a failed request on one short line, without the URL that net/http puts in front.
func httpErrText(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return "no answer in time"
		}
		err = ue.Err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "no answer in time"
	}
	return err.Error()
}

// bind binds addr and releases it at once: the real Env.Bind.
func bind(ctx context.Context, network, addr string) error {
	var lc net.ListenConfig
	if network == "udp" {
		c, err := lc.ListenPacket(ctx, "udp", addr)
		if err != nil {
			return err
		}
		return c.Close()
	}
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return ln.Close()
}

// wsaeAddrInUse is Winsock's "address already in use"; package syscall's EADDRINUSE is an invented value there.
const wsaeAddrInUse = syscall.Errno(10048)

func isAddrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE) || errors.Is(err, wsaeAddrInUse)
}

// checkPorts looks at the server's ports on this machine only (04 §13.2): which of them the running server
// listens on, or, offline, whether each can be bound. It says nothing about a firewall in front; the browser's
// connection test checks from outside. The result is therefore "info" at best, never "ok".
func checkPorts(ctx context.Context, r *run) result {
	p := params{}
	var good, disabled, disabledKeys, missing, inUse, inUseKeys, unchecked []string
	for _, l := range r.listeners() {
		if l.addr == "" {
			disabled = append(disabled, l.label())
			disabledKeys = append(disabledKeys, l.key)
			continue
		}
		if !r.offline() {
			if label, ok := r.liveListener(l); ok {
				good = append(good, label)
			} else {
				missing = append(missing, l.label())
			}
			continue
		}
		switch err := r.env.Bind(ctx, l.network, l.addr); {
		case err == nil:
			good = append(good, l.label())
		case isAddrInUse(err):
			inUse = append(inUse, l.label())
			inUseKeys = append(inUseKeys, l.key)
		default:
			// A privileged port that this user may not bind (the service gets CAP_NET_BIND_SERVICE from its
			// unit), or an address this machine does not have: nothing learned about the port.
			unchecked = append(unchecked, l.label())
			if errors.Is(err, fs.ErrPermission) {
				p["uncheckedDenied"] = true
			}
		}
	}
	set := func(key string, v []string) {
		if len(v) > 0 {
			p[key] = v
		}
	}
	if r.offline() {
		set("free", good)
	} else {
		set("listening", good)
	}
	set("disabled", disabled)
	set("disabledKeys", disabledKeys)
	set("missing", missing)
	set("inUse", inUse)
	set("inUseKeys", inUseKeys)
	set("unchecked", unchecked)
	switch {
	case len(inUse) > 0:
		return failResult(codePortsInUse, p, fixPortsFree)
	case len(missing) > 0:
		return warnResult(codePortsNotListening, p, "")
	case len(disabled) > 0:
		return warnResult(codePortsDisabled, p, "")
	case r.offline():
		return infoResult(codePortsFree, p)
	}
	return infoResult(codePortsListening, p)
}

// liveListener reports whether the running server listens for l, and the label of the port it bound (a configured
// port 0 shows as the port the kernel picked). The server lists its listeners by config key; the media ports also
// show in the addresses it advertises.
func (r *run) liveListener(l listener) (label string, ok bool) {
	for _, ln := range r.live.Listeners {
		if ln.Key == l.key {
			return portOf(ln.Addr, l.defaultPort()) + "/" + l.network, true
		}
	}
	via := map[string]api.Transport{"listen.ice_udp": api.TransportUDP, "listen.ice_tcp": api.TransportTCP7882}[l.key]
	if via == "" {
		return "", false
	}
	for _, a := range r.live.Advertised {
		if a.Via == via {
			return portOf(a.Addr, l.defaultPort()) + "/" + l.network, true
		}
	}
	return "", false
}

// checkFirewallHint tells the operator where to open the ports, in the words of the hosting provider that the
// machine's DMI strings name (04 §13.3). It checks nothing, so it works before the first start.
func checkFirewallHint(ctx context.Context, r *run) result {
	tcp, udp := r.publicPorts()
	p := params{"provider": string(r.host.Provider)}
	if len(tcp) > 0 {
		p["tcpPorts"] = tcp
	}
	if len(udp) > 0 {
		p["udpPorts"] = udp
	}
	if r.tlsMode() == config.TLSOff {
		p["proxy"] = true
	}
	if r.inContainer() && r.containerNetwork(ctx) == networkBridge {
		// Docker publishes ports with its own iptables rules, which ufw's rules never see.
		p["dockerBridge"] = true
	}
	return infoResult(codeFirewallHintOpenPorts, p)
}
