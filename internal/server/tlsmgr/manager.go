package tlsmgr

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caddyserver/certmagic"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/netx"
	"github.com/MoonWX/isshoni/internal/version"
)

// Options configures New. The wiring fills it from the config, the site and the listeners it bound (04 §8.2).
type Options struct {
	Mode   config.TLSMode
	Domain string // auto: the certificate's name
	// PublicIP is the server's public address (the IPv4 one, or the IPv6 one on an IPv6-only host): the
	// certificate's name in ip mode, and one of the addresses the domain should resolve to in auto mode. The zero
	// Addr means none is known; an ip-mode manager then has nothing to ask a certificate for and stays not ready.
	PublicIP netip.Addr
	// PublicIPv6 is the server's public IPv6 address when it has one next to PublicIP. Only auto mode's DNS check
	// reads it.
	PublicIPv6 netip.Addr
	Email      string // optional ACME account contact
	CA         string // the ACME directory; "" means Let's Encrypt
	Staging    bool   // Let's Encrypt's staging directory instead of its production one; no effect on another CA
	CARootFile string // tests: a PEM file trusted for the ACME server's own TLS (Pebble)
	CertFile   string // manual
	KeyFile    string // manual
	StorageDir string // auto, ip: <data_dir>/certmagic
	// HSTS is tls.hsts, listed here by 04 §8.2. The manager has no use for it: Strict-Transport-Security belongs
	// to HTTPS responses, which the router writes (04 §9.6), and never to a response on port 80 (RFC 6797 §7.2).
	HSTS   bool
	Logger *slog.Logger // nil means slog.Default(); the manager adds component=tls

	// The fields below are not in 04 §8.2's list; the manager needs them from the wiring all the same.

	// Site is the server's public address. Its Origin is where the port 80 handler redirects to (never the
	// request's Host), and manual mode warns when the certificate does not cover its Hostname.
	Site config.Site
	// HTTPAddr and HTTPSAddr are the bound addresses ("host:port") of the port 80 and the 443 listener. certmagic
	// answers challenges through them (HTTPHandler, TLSConfig); with their ports it finds the addresses taken and
	// opens none of its own. Empty means port 80 and port 443 on every address.
	HTTPAddr, HTTPSAddr string
	// Resolver looks the domain up for auto mode's DNS check. nil means net.DefaultResolver.
	Resolver netx.Resolver
}

// Status is the certificate's state, for the dashboard, doctor and the admin socket. api.TLSInfo is the same
// without LastError.
type Status struct {
	Mode          config.TLSMode `json:"mode"`
	Names         []string       `json:"names"` // what the certificate is for; never nil
	Ready         bool           `json:"ready"`
	Issuer        string         `json:"issuer,omitempty"`
	NotBefore     time.Time      `json:"not_before,omitzero"`
	NotAfter      time.Time      `json:"not_after,omitzero"`
	NextRenewal   time.Time      `json:"next_renewal,omitzero"`     // auto, ip: when the renewal window opens
	LastError     string         `json:"last_error,omitempty"`      // English, for logs and the CLI: the fix text
	LastErrorCode string         `json:"last_error_code,omitempty"` // a Code constant (04 §8.7)
	LastErrorAt   time.Time      `json:"last_error_at,omitzero"`
}

// ipRenewalWindowRatio makes a certificate of the shortlived profile (160 hours) renew at half its life: after
// about 80 hours, with more than three days of slack. Auto mode keeps certmagic's default, a third.
const ipRenewalWindowRatio = 0.5

// shortlivedProfile is the ACME profile Let's Encrypt requires for IP address identifiers.
const shortlivedProfile = "shortlived"

// dnsCheckTimeout bounds auto mode's lookup of its own domain.
const dnsCheckTimeout = 5 * time.Second

var (
	errStopped       = errors.New("tlsmgr: the manager was shut down; build a new one")
	errNoCertificate = errors.New("tlsmgr: no certificate yet")
)

// certmagic sends this with every ACME request. It is a package variable there, so it is set once.
var setUserAgent sync.Once

type lifeState uint8

const (
	stateNew lifeState = iota
	stateStarted
	stateStopped
)

// Manager is the TLS side of one server: the certificate for the 443 listener, the handler for port 80 and the
// readiness check "tls". New builds it and starts nothing; Start begins the work (the ACME management, or loading
// and watching the manual files) and Shutdown ends it. TLSConfig, HTTPHandler, Status and Ready may be called at any
// time, also before Start: there is no certificate then.
type Manager struct {
	opts Options
	log  *slog.Logger
	// name is the certificate's subject in auto and ip mode: the domain in lower case, or the IP address as text.
	// It is empty in the other modes, and in ip mode without a public address.
	name      string
	tlsConfig *tls.Config // nil in off mode
	// profile is the ACME profile the orders ask for: none in auto mode (the CA's default), "shortlived" in ip
	// mode. A test against Pebble sets it in auto mode too: Pebble gives an order without a profile a random one.
	profile string

	// Test seams, set before Start.
	now                func() time.Time
	pollInterval       time.Duration // manual: how often the files are checked for a change
	renewCheckInterval time.Duration // auto, ip: certmagic's maintenance tick; 0 means its default (10 min)
	noHTTP01           bool          // auto, ip: leave the http-01 challenge out
	noTLSALPN01        bool          // auto, ip: leave the tls-alpn-01 challenge out

	mu     sync.Mutex // the life cycle
	state  lifeState
	ctx    context.Context // ends at Shutdown; set by Start
	cancel context.CancelFunc
	wg     sync.WaitGroup // the manager's own goroutines; Add under mu while started

	acme    atomic.Pointer[acmeState]  // auto, ip: set by Start
	dnsOnce sync.Once                  // auto: the DNS check runs before the first order only
	manual  atomic.Pointer[loadedCert] // manual: the pair that serves
	loadMu  sync.Mutex                 // manual: one load at a time
	seen    [2]fileStamp               // manual: the files as the last load found them; under loadMu

	errMu       sync.Mutex
	lastErr     hint
	lastErrorAt time.Time
}

// acmeState is certmagic's side of an auto or ip manager.
type acmeState struct {
	storage *storage
	cache   *certmagic.Cache
	cfg     *certmagic.Config
	issuer  *certmagic.ACMEIssuer
}

// New checks opts and builds the manager. It reads no file and starts nothing.
func New(opts Options) (*Manager, error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	m := &Manager{
		opts:         opts,
		log:          log.With(slog.String("component", "tls")),
		now:          time.Now,
		pollInterval: manualPollInterval,
	}
	protos := []string{"h2", "http/1.1"}
	switch opts.Mode {
	case config.TLSOff:
		return m, nil
	case config.TLSAuto:
		if opts.Domain == "" {
			return nil, errors.New(`tlsmgr: tls.mode "auto" needs a domain`)
		}
		m.name = strings.ToLower(strings.TrimSuffix(opts.Domain, "."))
	case config.TLSIP:
		if opts.PublicIP.IsValid() {
			m.name = opts.PublicIP.Unmap().WithZone("").String()
		}
		// Let's Encrypt issues for an IP address only from its short-lived profile.
		m.profile = shortlivedProfile
	case config.TLSManual:
		if opts.CertFile == "" || opts.KeyFile == "" {
			return nil, errors.New(`tlsmgr: tls.mode "manual" needs tls.cert_file and tls.key_file`)
		}
	default:
		return nil, fmt.Errorf("tlsmgr: unknown tls.mode %q", opts.Mode)
	}
	if m.usesACME() {
		if opts.StorageDir == "" {
			return nil, fmt.Errorf("tlsmgr: tls.mode %q needs a storage directory for its certificates", opts.Mode)
		}
		// certmagic answers tls-alpn-01 challenges in GetCertificate, for clients that offer this protocol alone.
		protos = append(protos, "acme-tls/1")
	}
	m.tlsConfig = &tls.Config{
		MinVersion:     tls.VersionTLS12,
		NextProtos:     protos, // 04 §7.2
		GetCertificate: m.getCertificate,
	}
	return m, nil
}

// usesACME reports whether certmagic gets the certificate (auto and ip mode).
func (m *Manager) usesACME() bool {
	return m.opts.Mode == config.TLSAuto || m.opts.Mode == config.TLSIP
}

// Start begins the manager's work and returns at once: the certificate arrives in the background, and Ready tells
// when it is there.
//
//   - auto, ip: certmagic loads the certificate from StorageDir, or orders one, and renews it from then on. Failed
//     attempts are retried with certmagic's backoff; each one shows in Status.
//   - manual: the files are loaded and then checked for a change every minute. A pair that can't be loaded is no
//     error of Start: the server runs without a certificate, not ready, until good files appear (04 §8.4).
//   - off: nothing.
//
// ctx bounds the startup only; the work goes on until Shutdown. An error (StorageDir can't be created, CARootFile
// can't be used) leaves the manager unstarted. Start on a started manager does nothing.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch m.state {
	case stateStarted:
		return nil
	case stateStopped:
		return errStopped
	}
	run, cancel := context.WithCancel(context.WithoutCancel(ctx))
	m.ctx, m.cancel = run, cancel
	switch {
	case m.opts.Mode == config.TLSManual:
		_ = m.loadManual() // logged and kept in Status
		m.wg.Go(func() { m.watchManual(run) })
	case m.usesACME():
		if err := m.startACME(run); err != nil {
			cancel()
			return err
		}
	}
	m.state = stateStarted
	return nil
}

// Shutdown ends the manager's work: no further orders, renewals or reloads. The certificate that is loaded keeps
// serving the handshakes of connections that are still open.
//
// It cancels what is in flight and waits, until ctx ends, for the manager's goroutines, for certmagic's maintenance
// loop and for certmagic to let go of the storage. After a nil return nothing writes below StorageDir any more, so
// the directory may be replaced (a restore, 04 §12.4). Shutdown on a manager that never started, or again, does
// nothing.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	if m.state != stateStarted {
		m.state = stateStopped
		m.mu.Unlock()
		return nil
	}
	m.state = stateStopped
	cancel, st := m.cancel, m.acme.Load()
	m.mu.Unlock()

	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.wg.Wait()
		if st != nil {
			st.cache.Stop()
			<-st.storage.close()
		}
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("tlsmgr: shutdown: %w", ctx.Err())
	}
}

// TLSConfig returns the configuration of the 443 listener: TLS 1.2 or newer, the protocols of 04 §7.2 (h2,
// http/1.1, and acme-tls/1 where certmagic answers challenges) and the manager's certificate, whichever it is at the
// time of each handshake. A handshake that arrives while there is no certificate fails. It is nil in off mode. Each
// call returns a copy.
func (m *Manager) TLSConfig() *tls.Config {
	if m.tlsConfig == nil {
		return nil
	}
	c := m.tlsConfig.Clone()
	c.NextProtos = slices.Clone(c.NextProtos) // Clone shares the slice
	return c
}

func (m *Manager) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if m.opts.Mode == config.TLSManual {
		if c := m.manual.Load(); c != nil {
			return &c.cert, nil
		}
		return nil, errNoCertificate
	}
	if st := m.acme.Load(); st != nil {
		return st.cfg.GetCertificate(hello)
	}
	return nil, errNoCertificate
}

// Reload reads the certificate files again, as SIGHUP asks for (04 §6.5). A pair that can't be used is rejected
// with an error and the old one keeps serving; the error says what is wrong with the files and how to fix it. In
// the other modes there is nothing to reload.
func (m *Manager) Reload() error {
	if m.opts.Mode != config.TLSManual {
		return nil
	}
	return m.loadManual()
}

// Status returns the certificate's state.
func (m *Manager) Status() Status {
	st := Status{Mode: m.opts.Mode, Names: []string{}}
	if m.name != "" {
		st.Names = []string{m.name}
	}
	if m.opts.Mode == config.TLSOff {
		st.Ready = true
		return st
	}
	leaf, names := m.leaf()
	if leaf != nil {
		if names != nil {
			st.Names = names
		}
		st.Issuer = issuerName(leaf)
		st.NotBefore, st.NotAfter = leaf.NotBefore, leaf.NotAfter
		if m.usesACME() {
			window := time.Duration(float64(leaf.NotAfter.Sub(leaf.NotBefore)) * m.renewalWindowRatio())
			st.NextRenewal = leaf.NotAfter.Add(-window)
		}
	}
	st.Ready, _ = m.Ready()
	m.errMu.Lock()
	st.LastError, st.LastErrorCode, st.LastErrorAt = m.lastErr.fix, m.lastErr.code, m.lastErrorAt
	m.errMu.Unlock()
	return st
}

// Ready is the readiness check "tls" (04 §6.2): a certificate is loaded and the clock is inside its validity. Off
// mode is always ready. The detail of a manager that is not ready says what it waits for, with the code of the last
// error when there is one.
func (m *Manager) Ready() (bool, string) {
	if m.opts.Mode == config.TLSOff {
		return true, ""
	}
	leaf, _ := m.leaf()
	now := m.now()
	switch {
	case leaf == nil:
		return false, m.waitingFor()
	case now.After(leaf.NotAfter):
		return false, "the certificate expired on " + leaf.NotAfter.UTC().Format(time.DateOnly)
	case now.Before(leaf.NotBefore):
		return false, "the certificate is not valid before " + leaf.NotBefore.UTC().Format(time.DateOnly)
	}
	return true, ""
}

// waitingFor is Ready's detail while there is no certificate.
func (m *Manager) waitingFor() string {
	var what string
	switch {
	case m.opts.Mode == config.TLSManual:
		what = "no certificate loaded from tls.cert_file"
	case m.name == "":
		return "no public IP address to get a certificate for"
	default:
		what = "getting a certificate for " + m.name
	}
	m.errMu.Lock()
	code := m.lastErr.code
	m.errMu.Unlock()
	if code != "" {
		what += " (" + code + ")"
	}
	return what
}

// leaf returns the certificate that serves, with its names in manual mode, or nil.
func (m *Manager) leaf() (*x509.Certificate, []string) {
	if m.opts.Mode == config.TLSManual {
		if c := m.manual.Load(); c != nil {
			return c.leaf, c.names
		}
		return nil, nil
	}
	st := m.acme.Load()
	if st == nil {
		return nil, nil
	}
	// Around a renewal the cache may hold the old and the new certificate for a moment.
	var best *x509.Certificate
	for _, c := range st.cache.AllMatchingCertificates(m.name) {
		if c.Leaf != nil && (best == nil || c.Leaf.NotAfter.After(best.NotAfter)) {
			best = c.Leaf
		}
	}
	return best, nil
}

func (m *Manager) renewalWindowRatio() float64 {
	if m.opts.Mode == config.TLSIP {
		return ipRenewalWindowRatio
	}
	return certmagic.DefaultRenewalWindowRatio
}

func (m *Manager) setError(h hint) {
	m.errMu.Lock()
	m.lastErr, m.lastErrorAt = h, m.now()
	m.errMu.Unlock()
}

func (m *Manager) clearError() {
	m.errMu.Lock()
	m.lastErr, m.lastErrorAt = hint{}, time.Time{}
	m.errMu.Unlock()
}

// issuerName names a certificate's issuer for people: "Let's Encrypt E7", "Pebble Intermediate CA 5a3c1f".
func issuerName(leaf *x509.Certificate) string {
	org, cn := strings.Join(leaf.Issuer.Organization, ", "), leaf.Issuer.CommonName
	switch {
	case org == "":
		return firstNonEmpty(cn, leaf.Issuer.String())
	case cn == "" || strings.Contains(cn, org):
		return firstNonEmpty(cn, org)
	default:
		return org + " " + cn
	}
}

// startACME sets certmagic up as 04 §8.2 gives it and hands it the name. The caller holds m.mu.
func (m *Manager) startACME(ctx context.Context) error {
	if m.name == "" {
		m.log.Warn(`tls.mode "ip" has no public IP address to get a certificate for; set public_ip and restart`)
		return nil
	}
	roots, err := loadRoots(m.opts.CARootFile)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.opts.StorageDir, 0o700); err != nil {
		return fmt.Errorf("tlsmgr: certificate storage: %w", err)
	}
	setUserAgent.Do(func() { certmagic.UserAgent = version.UserAgent() })

	zl := logx.NewZapBridge(m.log)
	st := &acmeState{storage: newStorage(m.opts.StorageDir)}
	st.cache = certmagic.NewCache(certmagic.CacheOptions{
		// The maintenance loop asks at each tick, long after Start has stored the state.
		GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) {
			if cur := m.acme.Load(); cur != nil {
				return cur.cfg, nil
			}
			return nil, errNoCertificate
		},
		RenewCheckInterval: m.renewCheckInterval,
		Logger:             zl,
	})
	ratio := 0.0 // certmagic's default, a third of the lifetime
	if m.opts.Mode == config.TLSIP {
		ratio = ipRenewalWindowRatio
	}
	st.cfg = certmagic.New(st.cache, certmagic.Config{
		Storage: st.storage,
		// A client that connects to an IP address sends no SNI (RFC 6066 forbids IP literals there).
		DefaultServerName: m.name,
		// A client that asks for another name gets our certificate and a clear name-mismatch error, not a failed
		// handshake.
		FallbackServerName: m.name,
		RenewalWindowRatio: ratio,
		OnEvent:            m.onEvent,
		// Without this, certmagic builds an event for every handshake.
		ShouldEmitFunc: func(event string) bool {
			return event == eventObtaining || event == eventObtained || event == eventFailed
		},
		// No OCSP requests: Let's Encrypt has no OCSP any more, and the server makes no outbound request that the
		// privacy page does not list (04 §16).
		OCSP:   certmagic.OCSPConfig{DisableStapling: true},
		Logger: zl,
	})
	listenHost, httpPort, tlsPort := challengeListeners(m.opts.HTTPAddr, m.opts.HTTPSAddr)
	st.issuer = certmagic.NewACMEIssuer(st.cfg, certmagic.ACMEIssuer{
		CA:           m.directory(),
		Email:        m.opts.Email,
		Agreed:       true, // install.sh and the docs tell the admin that the CA's subscriber agreement applies (06)
		Profile:      m.profile,
		TrustedRoots: roots,
		// certmagic binds a challenge port only when it is free. Ours are bound before Start, so naming them here
		// is what keeps certmagic from opening ports of its own: the challenges arrive at HTTPHandler and TLSConfig.
		ListenHost:              listenHost,
		AltHTTPPort:             httpPort,
		AltTLSALPNPort:          tlsPort,
		DisableHTTPChallenge:    m.noHTTP01,
		DisableTLSALPNChallenge: m.noTLSALPN01,
		Logger:                  zl,
	})
	st.cfg.Issuers = []certmagic.Issuer{st.issuer}
	m.acme.Store(st)
	if err := st.cfg.ManageAsync(ctx, []string{m.name}); err != nil {
		m.acme.Store(nil)
		st.cache.Stop()
		return fmt.Errorf("tlsmgr: managing the certificate for %s: %w", m.name, err)
	}
	return nil
}

// directory returns the ACME directory URL: CA, or Let's Encrypt, whose staging directory Staging selects.
func (m *Manager) directory() string {
	ca := m.opts.CA
	if ca == "" {
		ca = certmagic.LetsEncryptProductionCA
	}
	if m.opts.Staging && ca == certmagic.LetsEncryptProductionCA {
		ca = certmagic.LetsEncryptStagingCA
	}
	return ca
}

// challengeListeners turns the bound addresses of the port 80 and the 443 listener into certmagic's terms: one
// listen host for both, and the two ports (0 means certmagic's default, 80 and 443). certmagic has a single host
// field; when the two listeners differ in theirs it gets none, which is every address.
func challengeListeners(httpAddr, httpsAddr string) (host string, httpPort, tlsPort int) {
	split := func(addr string) (string, int) {
		h, p, err := net.SplitHostPort(addr)
		if err != nil {
			return "", 0
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 65535 {
			return "", 0
		}
		return h, n
	}
	httpHost, httpPort := split(httpAddr)
	tlsHost, tlsPort := split(httpsAddr)
	if httpHost == tlsHost {
		host = httpHost
	}
	return host, httpPort, tlsPort
}

// loadRoots reads the PEM file of tls.acme_ca_root. "" means the system's roots (a nil pool).
func loadRoots(file string) (*x509.CertPool, error) {
	if file == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(file) //nolint:gosec // G304: the operator's own path, from tls.acme_ca_root
	if err != nil {
		return nil, fmt.Errorf("tlsmgr: tls.acme_ca_root: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("tlsmgr: tls.acme_ca_root = %q holds no PEM certificate", file)
	}
	return pool, nil
}

// certmagic's events that the manager listens to.
const (
	eventObtaining = "cert_obtaining" // an order or a renewal begins
	eventObtained  = "cert_obtained"  // the certificate is in storage
	eventFailed    = "cert_failed"    // an attempt failed; certmagic retries with its backoff
)

// onEvent is certmagic's event callback. It runs on certmagic's goroutine, so it returns quickly; its nil result
// lets certmagic go on.
func (m *Manager) onEvent(_ context.Context, event string, data map[string]any) error {
	renewal, _ := data["renewal"].(bool)
	switch event {
	case eventObtaining:
		if m.opts.Mode == config.TLSAuto && !renewal {
			m.dnsOnce.Do(m.startDNSCheck)
		}
	case eventObtained:
		m.clearError()
		m.log.Info("certificate obtained", slog.String("name", m.name), slog.Bool("renewal", renewal))
	case eventFailed:
		err, _ := data["error"].(error)
		if err == nil {
			err = errors.New("the certificate authority gave no reason")
		}
		if m.stopping() {
			// Shutdown cut the attempt off. That is no problem of the certificate's, and nobody retries.
			m.log.Debug("certificate request cancelled by the shutdown", slog.String("name", m.name), logx.Err(err))
			return nil
		}
		h := hintFor(err, hintEnv{name: m.name, isIP: m.opts.Mode == config.TLSIP, publicIP: m.opts.PublicIP})
		m.setError(h)
		msg := "could not get a certificate; trying again later"
		if renewal {
			msg = "could not renew the certificate; trying again later"
		}
		m.log.Warn(msg, slog.String("name", m.name), slog.String("code", h.code), slog.String("fix", h.fix), logx.Err(err))
	}
	return nil
}

// stopping reports whether Shutdown has begun.
func (m *Manager) stopping() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state == stateStopped
}

// startDNSCheck runs the DNS check in a goroutine of the manager, unless the manager is stopping.
func (m *Manager) startDNSCheck() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != stateStarted {
		return
	}
	ctx := m.ctx
	m.wg.Go(func() { m.checkDNS(ctx) })
}

// checkDNS is auto mode's early look at its own domain, before the first order (04 §8.7): when the domain does not
// resolve, or resolves to other addresses than this server's, the log says so at once, with the fix. The order goes
// ahead regardless: DNS may be split-horizon, and the CA's view is what counts.
func (m *Manager) checkDNS(ctx context.Context) {
	var own []netip.Addr
	for _, a := range []netip.Addr{m.opts.PublicIP, m.opts.PublicIPv6} {
		if a.IsValid() {
			own = append(own, a.Unmap().WithZone(""))
		}
	}
	if len(own) == 0 {
		return // nothing to compare with
	}
	resolver := m.opts.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	ctx, cancel := context.WithTimeout(ctx, dnsCheckTimeout)
	defer cancel()
	found, err := resolver.LookupNetIP(ctx, "ip", m.name)
	var dnsErr *net.DNSError
	switch {
	case errors.As(err, &dnsErr) && dnsErr.IsNotFound:
		m.log.Warn("the domain does not resolve", slog.String("name", m.name), slog.String("code", CodeDNSMissing),
			slog.String("fix", m.name+" doesn't resolve. Add an A record pointing to "+own[0].String()+"."))
		return
	case err != nil:
		m.log.Debug("could not look the domain up", slog.String("name", m.name), logx.Err(err))
		return
	}
	for _, a := range found {
		for _, o := range own {
			if a.Unmap().WithZone("") == o {
				return
			}
		}
	}
	other := "no address"
	if len(found) > 0 {
		other = found[0].Unmap().String()
	}
	m.log.Warn("the domain does not point to this server", slog.String("name", m.name), slog.String("code", CodeDNSWrong),
		slog.String("fix", m.name+" points to "+other+", but this server is "+own[0].String()+"."))
}
