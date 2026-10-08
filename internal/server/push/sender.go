package push

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/server/config"
)

// Sender delivers one message to one subscription: it encrypts the payload for the subscription's keys and POSTs
// it to the subscription's endpoint. The real one (Options.Sender nil) is webpush-go with the guarded HTTP client
// of 04 §14.5; tests and servertest inject fakes (server.Deps.PushSender).
//
// Send returns the push service's answer as a SendResult with a nil error, whatever the status: the Service
// applies the result table of 04 §14.5. An error means no answer arrived: the Service retries it like a 5xx,
// unless the error wraps ErrUndeliverable. Errors must not contain the endpoint (it is a capability URL).
type Sender interface {
	Send(ctx context.Context, sub Subscription, payload []byte, o SendOptions) (SendResult, error)
}

// SendOptions are the per-message headers of RFC 8030.
type SendOptions struct {
	TTL     time.Duration // how long the push service keeps the message for an offline device (whole seconds)
	Urgency string        // "very-low" | "low" | "normal" | "high" (the Urgency* constants)
	Topic   string        // a newer message with the same topic replaces a pending one; "" for none
}

// Urgency values of SendOptions.Urgency (RFC 8030 §5.3).
const (
	UrgencyVeryLow = "very-low"
	UrgencyLow     = "low"
	UrgencyNormal  = "normal"
	UrgencyHigh    = "high"
)

// SendResult is the push service's answer.
type SendResult struct {
	Status     int           // the HTTP status
	RetryAfter time.Duration // from a Retry-After header; 0 when there was none
}

// ErrUndeliverable is wrapped by a Sender's error when trying again can't help: the subscription's keys don't
// decode, its endpoint breaks the URL rules, or the guard refused the address. The Service counts a failure for
// the subscription and does not retry.
var ErrUndeliverable = errors.New("push: the message can't be delivered to this subscription")

// ErrBlockedAddress is the SSRF guard's refusal (04 §14.5): the push service's name resolved to an address that
// is not public, is this server's own, or the port is not 443. It wraps ErrUndeliverable.
var ErrBlockedAddress = fmt.Errorf("%w: the SSRF guard refused the address", ErrUndeliverable)

// Limits of the guarded HTTP client (04 §14.5).
const (
	sendTimeout     = 10 * time.Second // one request, from dial to the end of the body
	maxResponseBody = 4 << 10          // read at most this much of a response body
	maxRetryAfter   = 60 * time.Second // a longer Retry-After is not waited for (the Service gives up instead)
)

// webPushSender is the real Sender: RFC 8291 encryption and the RFC 8292 VAPID header by webpush-go, sent through
// an http.Client that only reaches public addresses on port 443, uses no proxy and follows no redirect.
type webPushSender struct {
	client     *http.Client
	transport  *http.Transport
	publicKey  string
	privateKey logx.Secret
	subscriber string // the subject in the form webpush-go takes it
	lax        bool   // tests: guard.allowPrivate
	// dial is transport's DialContext. Tests replace it to stand in for DNS (a name that now resolves elsewhere).
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// newWebPushSender builds the real Sender. roots is nil outside tests (the system's root certificates).
func newWebPushSender(keys config.VAPIDKeys, subject string, g *guard, roots *x509.CertPool) *webPushSender {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second, Control: g.control}
	w := &webPushSender{
		publicKey:  keys.Public,
		privateKey: keys.Private,
		// webpush-go puts "mailto:" in front of every subject that is not an https: URL.
		subscriber: strings.TrimPrefix(subject, "mailto:"),
		lax:        g.allowPrivate,
		dial:       dialer.DialContext,
	}
	w.transport = &http.Transport{
		Proxy: nil, // never through a proxy, whatever the environment says: the guard checks the dialed address
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return w.dial(ctx, network, addr)
		},
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		ForceAttemptHTTP2:      true,
		TLSHandshakeTimeout:    5 * time.Second,
		ResponseHeaderTimeout:  sendTimeout,
		MaxResponseHeaderBytes: 16 << 10,
		MaxIdleConns:           16,
		MaxIdleConnsPerHost:    4,
		IdleConnTimeout:        90 * time.Second,
	}
	w.client = &http.Client{
		Transport: w.transport,
		Timeout:   sendTimeout,
		// No redirects: a push service never needs one, and a redirect would be a second, unchecked URL.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return w
}

// Close drops the idle connections to the push services.
func (w *webPushSender) Close() { w.transport.CloseIdleConnections() }

// Send implements Sender.
func (w *webPushSender) Send(ctx context.Context, sub Subscription, payload []byte, o SendOptions) (SendResult, error) {
	endpoint := sub.Endpoint.Reveal()
	// The stored endpoint passed ValidateEndpoint when the browser subscribed; checking the URL rules again costs
	// nothing and keeps a row written any other way from reaching the client.
	u, reason := parseEndpoint(endpoint, w.lax)
	if reason != "" {
		return SendResult{}, fmt.Errorf("%w: its endpoint is refused (%s)", ErrUndeliverable, reason)
	}
	host := u.Hostname()
	endpoint = canonicalEndpoint(endpoint, u)
	doer := &onceDoer{client: w.client}
	resp, err := webpush.SendNotificationWithContext(ctx, payload,
		&webpush.Subscription{Endpoint: endpoint, Keys: webpush.Keys{P256dh: sub.P256dh, Auth: sub.Auth}},
		&webpush.Options{
			HTTPClient:      doer,
			Subscriber:      w.subscriber,
			VAPIDPublicKey:  w.publicKey,
			VAPIDPrivateKey: w.privateKey.Reveal(),
			TTL:             int(o.TTL / time.Second),
			Urgency:         webpush.Urgency(o.Urgency),
			Topic:           o.Topic,
		})
	if err != nil {
		err = withoutURL(err)
		if !doer.called {
			// webpush-go failed before sending anything: the subscription's keys can't be used.
			return SendResult{}, fmt.Errorf("%w: encrypting for %s: %w", ErrUndeliverable, host, err)
		}
		return SendResult{}, fmt.Errorf("push: sending to %s: %w", host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
	return SendResult{
		Status:     resp.StatusCode,
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
	}, nil
}

// canonicalEndpoint returns the endpoint raw (parsed as u) with its origin in the form of RFC 6454: a lower-case
// host and no default port. webpush-go builds the token's aud claim from the URL as it is written, and RFC 8292
// wants the origin there: to a strict push service "https://Host:443" is another audience than "https://host".
// Browsers give the canonical form, and such an endpoint is returned as it is, byte for byte.
func canonicalEndpoint(raw string, u *url.URL) string {
	if strings.HasPrefix(u.Host, "[") {
		return raw // an IPv6 literal: only the tests' guard lets one through
	}
	host := strings.ToLower(u.Hostname())
	if port := u.Port(); port != "" && port != "443" {
		host += ":" + port
	}
	if host == u.Host {
		return raw
	}
	c := *u
	c.Host = host
	return c.String()
}

// onceDoer is the webpush.HTTPClient of one Send. It records whether the request went out, which tells a failure
// of the encryption (never retried) from a failure of the network (retried).
type onceDoer struct {
	client *http.Client
	called bool
}

func (d *onceDoer) Do(req *http.Request) (*http.Response, error) {
	d.called = true
	// The URL is a browser's (untrusted) on purpose: this client is the SSRF guard. Send checked the URL rules,
	// and the client's dialer refuses every address that is not public (guard.control).
	return d.client.Do(req) //nolint:gosec // G704: see above
}

// withoutURL removes the request URL from an HTTP client error. net/http reports failures as *url.Error, whose
// text starts with the full URL: here that is the subscription endpoint, which must never reach a log.
func withoutURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// parseRetryAfter reads a Retry-After header: a number of seconds or an HTTP date. 0 means none (or one that
// can't be read, or that is already over).
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n <= 0 {
			return 0
		}
		return time.Duration(min(n, 24*60*60)) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return min(d, 24*time.Hour)
		}
	}
	return 0
}
