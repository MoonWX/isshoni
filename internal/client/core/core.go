package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/client/signal"
	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/version"
)

// Options configure a Client (docs/m2/01-client-core.md §11.2).
type Options struct {
	// ServerURL is the linked server's origin: "https://watch.example.com" (02). http only for 02's loopback dev
	// origins (localhost, 127.0.0.1, ::1).
	ServerURL string
	// Auth is 02's *deviceauth.Manager; nil only with Header.
	Auth Auth
	// Header is for tests and tools only: a Cookie instead of a bearer token. It goes with every upgrade request.
	Header http.Header
	// Client goes into every hello: {Kind: desktop, Version: version.Version(), OS: windows}. Zero fields take
	// those defaults (OS from the platform the core runs on).
	Client protocol.ClientInfo
	// Engine is the media engine: internal/media/native on Windows, internal/media/fake in tests and on macOS and
	// Linux.
	Engine media.Engine
	// AudioConfig returns the audio start config (05's media.AudioConfig: mode, rules, SharedPID); 03 wires it to
	// 05's Service.AudioConfig(). The core calls it at each share's Engine.Start and overwrites BitrateBPS with the
	// share's ShareParams.AudioBitrate. nil = {Mode: media.AudioExcludeApps} (tests).
	AudioConfig func() media.AudioConfig
	// Encoder "" is automatic; a probe token forces that kind (03's hidden support override, 04 §20).
	Encoder string
	// Settings are the Pion settings of the pub PeerConnection; nil = DefaultSettings(Logger).
	Settings *webrtc.SettingEngine
	// HTTPClient makes the WebSocket upgrade requests; nil = http.DefaultClient.
	HTTPClient *http.Client
	// Logger gets the core's logs (§11.12: never a token, an SDP, a candidate or a source name); nil discards them.
	Logger *slog.Logger
	// allowListed is for in-package tests only (TestCaptureToSend): Engine.Start gets Config.Listed.
	allowListed bool
}

// Auth is exactly what 02's *deviceauth.Manager provides.
type Auth interface {
	// AccessToken returns an access token, normally with at least 120 s left (02 §11.4); while a refresh is being
	// retried, possibly as little as 10 s, which is enough for one hello, since the token is checked once (M1 01
	// §3.2). forceRefresh drops the token the server refused; 02 does at most one forced refresh per 10 s and returns
	// its result to later forced calls. An error whose chain has a RelinkRequired() bool method returning true means
	// the device is no longer linked; one whose chain has UpdateRequired() bool returning true means the app and the
	// server are incompatible (02 §13); any other error is transient.
	AccessToken(ctx context.Context, forceRefresh bool) (protocol.Secret, error)
	// SessionEnded reports a stop in scope session (session_revoked, account_disabled) or a second unauthenticated
	// right after a forced refresh. The manager refreshes once and, on invalid_grant, deletes the credential and sets
	// its notice (signed_out, account_disabled); it owns the credential and the link state.
	SessionEnded(ctx context.Context, code protocol.ErrorCode)
}

// The errors of the API (§11.2).
var (
	ErrBusy     = errors.New("core: a share is active or starting")
	ErrNotReady = errors.New("core: not connected")
	ErrClosed   = errors.New("core: closed")
)

// errNotImplemented is returned by the parts of the API that later slices of docs/m2/README.md fill in.
var errNotImplemented = errors.New("core: not implemented yet")

// StopError is why Run ended without its ctx ending: a terminal stop of the connection (§11.3).
type StopError struct {
	Reason StopReason
	Code   protocol.ErrorCode // the server's code when it said one
	Err    *protocol.Error    // the server's error as signal.StateChange.Err reported it (params included); nil for local stops
}

// Error returns "core: stopped: <reason>", with the server's code when there is one.
func (e *StopError) Error() string {
	if e.Code != "" {
		return "core: stopped: " + string(e.Reason) + " (" + string(e.Code) + ")"
	}
	return "core: stopped: " + string(e.Reason)
}

// StopReason is the reason of a StopError.
type StopReason string

// The stop reasons (§11.3's table).
const (
	StopUnlinked       StopReason = "unlinked" // session_revoked, a second unauthenticated right after the forced refresh, or an Auth error with RelinkRequired
	StopDisabled       StopReason = "account_disabled"
	StopUpdateRequired StopReason = "update_required" // protocol_unsupported, client_outdated, or an Auth error with UpdateRequired
	StopTooMany        StopReason = "too_many_connections"
	StopReplaced       StopReason = "replaced" // another socket resumed this connection (a second app instance)
	StopProtocol       StopReason = "bad_message"
)

// Timeouts of the core (§13).
const (
	// closeTimeout bounds the wait for the share.stop reply in Close and at the end of Run.
	closeTimeout = 3 * time.Second
	// sessionEndedTimeout bounds Auth.SessionEnded.
	sessionEndedTimeout = 15 * time.Second
	// cleanupTimeout bounds each request of an aborted start or a share's end that has no caller's ctx.
	cleanupTimeout = 3 * time.Second
)

// DefaultSettings returns the Pion settings of §11.6: UDP and TCP (ICE-TCP to the server's 443/7882), IPv4 and
// IPv6, no mDNS, no loopback candidates, ICE timeouts disconnected 3 s / failed 25 s / keepalive 1 s, and logx's
// Pion logger (it redacts ICE passwords).
func DefaultSettings(log *slog.Logger) *webrtc.SettingEngine {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	se := &webrtc.SettingEngine{}
	se.SetNetworkTypes([]webrtc.NetworkType{
		webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6, webrtc.NetworkTypeTCP4, webrtc.NetworkTypeTCP6,
	})
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	se.SetIncludeLoopbackCandidate(false)
	se.SetICETimeouts(3*time.Second, 25*time.Second, time.Second)
	se.LoggerFactory = logx.NewPionLoggerFactory(log)
	return se
}

// Client is the desktop app's Go connection to its linked server (§11): the publisher connection, its shares, the
// media pump and the events for the app. Its methods are safe for concurrent use.
type Client struct {
	o        Options
	log      *slog.Logger
	wsURL    string
	settings webrtc.SettingEngine
	events   subscribers

	// life ends with Close and when Run ends: it ends StartShare and StopOther calls in flight.
	life       context.Context
	cancelLife context.CancelFunc
	done       chan struct{}   // closed when Run has returned
	authStop   chan *StopError // one slot: a terminal Auth error of a reconnect's token call (token)
	roomMu     sync.Mutex      // serializes the room operations of StartShare, StopOther and a share's end
	ops        sync.WaitGroup  // StartShare and StopOther calls in flight

	mu         sync.Mutex // guards the fields below
	ran        bool       // Run was called
	closed     bool       // Close was called
	status     Status     // Share is always nil here: Status() fills it in
	sig        *signal.Client
	welcomed   bool // a welcome arrived, on this or an earlier signaling client
	room       roomView
	share      *Share
	authErr    *StopError         // the first terminal Auth error of a dial
	dialCancel context.CancelFunc // ends the Dial in progress
	forced     *protocol.Secret   // the forced refresh's token, for the next hello only
	watchStats bool
}

// New checks o and returns a Client. It starts nothing.
func New(o Options) (*Client, error) {
	u, err := url.Parse(o.ServerURL)
	if err != nil || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("core: options: ServerURL %q is not an origin", o.ServerURL)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !loopbackHost(u.Hostname()) {
			return nil, fmt.Errorf("core: options: ServerURL %q: http only for a loopback host", o.ServerURL)
		}
	default:
		return nil, fmt.Errorf("core: options: ServerURL %q: want https", o.ServerURL)
	}
	switch {
	case o.Auth == nil && o.Header == nil:
		return nil, errors.New("core: options: no Auth and no Header")
	case o.Engine == nil:
		return nil, errors.New("core: options: no Engine")
	}
	o.Header = o.Header.Clone()
	if o.Client.Kind == "" {
		o.Client.Kind = protocol.ClientKindDesktop
	}
	if o.Client.Version == "" {
		o.Client.Version = version.Version()
	}
	if o.Client.OS == "" {
		o.Client.OS = clientOS()
	}
	log := o.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	settings := o.Settings
	if settings == nil {
		settings = DefaultSettings(log)
	}
	c := &Client{
		o:        o,
		log:      log,
		wsURL:    strings.TrimSuffix(o.ServerURL, "/") + "/ws",
		settings: *settings,
		done:     make(chan struct{}),
		authStop: make(chan *StopError, 1),
		status:   Status{Conn: ConnConnecting},
		room:     roomView{changed: make(chan struct{})},
	}
	c.life, c.cancelLife = context.WithCancel(context.Background())
	return c, nil
}

// loopbackHost reports whether host is localhost or a loopback IP address.
func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// clientOS is hello's client.os for the platform the core runs on.
func clientOS() protocol.ClientOS {
	switch runtime.GOOS {
	case "windows":
		return protocol.ClientOSWindows
	case "darwin":
		return protocol.ClientOSMacOS
	case "linux":
		return protocol.ClientOSLinux
	case "ios":
		return protocol.ClientOSIOS
	case "android":
		return protocol.ClientOSAndroid
	}
	return protocol.ClientOSOther
}

// Run connects and keeps the connection until ctx ends or a terminal stop (Status.Stop set); then it ends the share
// and returns. The error is nil when ctx ended, else a *StopError. A Client runs once: Run after it has returned, or
// after Close, returns ErrClosed. The app makes a new Client per (server, link) and calls Close before switching
// (server switch, relink, sign-out). Cancelling Run's ctx runs the same steps as Close, and Run returns when they
// are done (the same 3 s bound).
func (c *Client) Run(ctx context.Context) error {
	c.mu.Lock()
	if c.ran || c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	c.ran = true
	c.mu.Unlock()
	defer close(c.done)
	defer c.cancelLife()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopWatch := context.AfterFunc(c.life, cancel) // Close
	defer stopWatch()

	stop, ended := c.connect(runCtx, c.helloCaps(runCtx))
	c.cancelLife() // StartShare and StopOther calls in flight end now
	if stop == nil {
		c.shutdown()
		return nil
	}
	c.terminate(stop, ended)
	return stop
}

// Status returns the connection's state, with the active share's snapshot.
func (c *Client) Status() Status {
	c.mu.Lock()
	st := c.status
	sh := c.share
	c.mu.Unlock()
	st = *copyStatus(st)
	if sh != nil {
		snap := sh.Snapshot()
		st.Share = &snap
	}
	return st
}

// Subscribe returns a channel of events (64 deep; when a subscriber falls behind, its oldest events are dropped
// and EventResync tells it to read Status again) and a function that ends the subscription and closes the channel.
func (c *Client) Subscribe() (<-chan Event, func()) {
	return c.events.add()
}

// ActiveShare returns the share that is starting or running, nil when none.
func (c *Client) ActiveShare() *Share {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.share
}

// Close is idempotent: it sends share.stop for a live share and stops the engine session (capture ends within one
// frame interval, 04 §6.3), waits at most 3 s for the share.stop reply, then closes with 1000 (the server ends
// anything left at once, 01 §4.2). It returns within 3 s plus the close handshake; 03's Quit waits at most 3 s.
func (c *Client) Close() error {
	c.mu.Lock()
	c.closed = true
	ran := c.ran
	c.mu.Unlock()
	c.cancelLife()
	if ran {
		<-c.done
	} else {
		c.setConn(ConnStopped) // Run will refuse to run
	}
	return nil
}

// setStatus changes the status and fires EventStatus when a field of it changed.
func (c *Client) setStatus(change func(*Status)) {
	c.mu.Lock()
	before := c.status
	change(&c.status)
	after := c.status
	sh := c.share
	c.mu.Unlock()
	if sameStatus(before, after) {
		return
	}
	var snap *ShareSnapshot
	if sh != nil {
		s := sh.Snapshot()
		snap = &s
	}
	c.events.publish(func() Event {
		st := after
		st.Share = snap
		return Event{Kind: EventStatus, Status: copyStatus(st)}
	})
}

// setConn sets Status.Conn.
func (c *Client) setConn(conn string) {
	c.setStatus(func(st *Status) { st.Conn = conn })
}

// isOnline reports whether signaling is ready.
func (c *Client) isOnline() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status.Conn == ConnOnline && c.sig != nil
}
