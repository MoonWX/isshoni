package push

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// userAgent is a browser's side of a subscription: the P-256 key pair and the auth secret of RFC 8291.
type userAgent struct {
	key  *ecdh.PrivateKey
	auth []byte
}

func newUserAgent(t testing.TB) *userAgent {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	return &userAgent{key: key, auth: auth}
}

// subscription is what the browser's PushSubscription.toJSON() gives, as the wiring hands it to the Service.
func (ua *userAgent) subscription(id, userID, endpoint string) Subscription {
	return Subscription{
		ID: id, UserID: userID, SessionID: "sess-" + id,
		Endpoint: logx.Secret(endpoint),
		P256dh:   b64(ua.key.PublicKey().Bytes()),
		Auth:     b64(ua.auth),
	}
}

// decrypt opens an aes128gcm message (RFC 8188) encrypted for this user agent (RFC 8291 §3.3 and §3.4), the way a
// browser does: only the subscription's private key and auth secret can. It returns the plaintext without its
// padding, and the record size from the header.
func (ua *userAgent) decrypt(body []byte) (plain []byte, recordSize uint32, err error) {
	if len(body) < 21 {
		return nil, 0, errors.New("shorter than the aes128gcm header")
	}
	salt := body[:16]
	recordSize = binary.BigEndian.Uint32(body[16:20])
	idLen := int(body[20])
	if len(body) < 21+idLen {
		return nil, 0, errors.New("the key id is cut off")
	}
	keyID, ciphertext := body[21:21+idLen], body[21+idLen:]
	if len(ciphertext) > int(recordSize) {
		return nil, 0, errors.New("more than one record")
	}

	// The key id is the application server's ephemeral public key.
	serverKey, err := ecdh.P256().NewPublicKey(keyID)
	if err != nil {
		return nil, 0, fmt.Errorf("key id: %w", err)
	}
	secret, err := ua.key.ECDH(serverKey)
	if err != nil {
		return nil, 0, err
	}
	keyInfo := "WebPush: info\x00" + string(ua.key.PublicKey().Bytes()) + string(keyID)
	ikm, err := hkdf.Key(sha256.New, secret, ua.auth, keyInfo, 32)
	if err != nil {
		return nil, 0, err
	}
	cek, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, 0, err
	}
	nonce, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, 0, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, 0, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, 0, err
	}
	plain, err = gcm.Open(nil, nonce, ciphertext, nil) // one record: its nonce is the base nonce (sequence 0)
	if err != nil {
		return nil, 0, fmt.Errorf("AES-GCM: %w", err)
	}
	// The last record ends in the delimiter 0x02 and zero padding.
	end := len(plain) - 1
	for end >= 0 && plain[end] == 0 {
		end--
	}
	if end < 0 || plain[end] != 2 {
		return nil, 0, errors.New("no padding delimiter")
	}
	return plain[:end], recordSize, nil
}

// vapidClaims are the claims of an RFC 8292 token.
type vapidClaims struct {
	Aud string `json:"aud"`
	Exp int64  `json:"exp"`
	Sub string `json:"sub"`
}

// verifyVAPID checks an RFC 8292 Authorization header the way a push service does: "vapid t=<JWT>, k=<key>", an
// ES256 signature by the key in k. It returns the claims and k.
func verifyVAPID(authorization string) (vapidClaims, string, error) {
	var c vapidClaims
	rest, ok := strings.CutPrefix(authorization, "vapid ")
	if !ok {
		return c, "", fmt.Errorf("scheme of %q is not vapid", authorization)
	}
	var token, key string
	for _, part := range strings.Split(rest, ",") {
		name, value, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch name {
		case "t":
			token = value
		case "k":
			key = value
		}
	}
	seg := strings.Split(token, ".")
	if len(seg) != 3 {
		return c, key, errors.New("the token is not a JWT")
	}
	var header struct{ Alg, Typ string }
	if err := decodeSegment(seg[0], &header); err != nil {
		return c, key, err
	}
	if header.Alg != "ES256" || header.Typ != "JWT" {
		return c, key, fmt.Errorf("JWT header %+v, want ES256 JWT", header)
	}
	pub, err := base64.RawURLEncoding.DecodeString(key)
	if err != nil {
		return c, key, err
	}
	pk, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), pub)
	if err != nil {
		return c, key, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(seg[2])
	if err != nil || len(sig) != 64 {
		return c, key, errors.New("the signature is not 64 bytes")
	}
	digest := sha256.Sum256([]byte(seg[0] + "." + seg[1]))
	if !ecdsa.Verify(pk, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return c, key, errors.New("the signature does not verify with k")
	}
	return c, key, decodeSegment(seg[1], &c)
}

func decodeSegment(s string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// pushRequest is one POST that the fake push service received.
type pushRequest struct {
	Method string
	Path   string
	URI    string // the request target as it was sent
	Host   string
	Header http.Header
	Body   []byte
}

// pushService is a fake push service: an HTTPS server on loopback that records each message and answers 201, or
// what respond says.
type pushService struct {
	srv     *httptest.Server
	got     chan pushRequest
	respond func(w http.ResponseWriter, r *http.Request) // nil: 201 Created
}

func newPushService(t testing.TB) *pushService {
	t.Helper()
	ps := &pushService{got: make(chan pushRequest, 16)}
	ps.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ps.got <- pushRequest{
			Method: r.Method, Path: r.URL.Path, URI: r.RequestURI, Host: r.Host, Header: r.Header.Clone(), Body: body,
		}
		if ps.respond != nil {
			ps.respond(w, r)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	ps.srv.Config.ErrorLog = slog.NewLogLogger(slog.DiscardHandler, slog.LevelError) // failed handshakes are expected
	ps.srv.EnableHTTP2 = true
	ps.srv.StartTLS()
	t.Cleanup(ps.srv.Close)
	return ps
}

// roots are the certificates that verify the fake push service.
func (ps *pushService) roots() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(ps.srv.Certificate())
	return pool
}

func (ps *pushService) next(t testing.TB) pushRequest {
	t.Helper()
	select {
	case r := <-ps.got:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("the fake push service got no request")
		return pushRequest{}
	}
}

// realSenderOptions makes the Service build the real sender, pointed at the fake push service: loopback and its
// random port are allowed, and its certificate is trusted.
func realSenderOptions(ps *pushService) func(*Options) {
	return func(o *Options) {
		o.Sender = nil
		o.allowPrivate = true
		o.rootCAs = ps.roots()
	}
}

// waitFor polls until cond holds.
func waitFor(t testing.TB, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The whole path with the real sender: a trigger becomes an RFC 8291 message that only the subscription's private
// key decrypts, carried by a request with the headers of RFC 8030 and a VAPID token that verifies (04 §17).
func TestRoundTripThroughFakePushService(t *testing.T) {
	for _, subject := range []string{"mailto:admin@example.com", "https://watch.example.com"} {
		t.Run(subject, func(t *testing.T) {
			ps := newPushService(t)
			ua, other := newUserAgent(t), newUserAgent(t)
			endpoint := ps.srv.URL + "/wpush/v2/SECRET-CAPABILITY-TOKEN"
			bob := ua.subscription("sub1", "bob", endpoint)
			f := newFixture(t, func(o *Options) {
				realSenderOptions(ps)(o)
				o.Subject = subject
			}, bob)
			stop := f.start(t)
			wantAud := ps.srv.URL // scheme://host:port of the endpoint

			check := func(wantTTL, wantUrgency, wantTopic string, wantPayload api.PushPayload) {
				t.Helper()
				before := time.Now()
				r := ps.next(t)
				if r.Method != http.MethodPost || r.Path != "/wpush/v2/SECRET-CAPABILITY-TOKEN" {
					t.Errorf("request %s %s, want POST to the endpoint", r.Method, r.Path)
				}
				for name, want := range map[string]string{
					"Content-Encoding": "aes128gcm",
					"Content-Type":     "application/octet-stream",
					"TTL":              wantTTL,
					"Urgency":          wantUrgency,
					"Topic":            wantTopic,
				} {
					if got := r.Header.Get(name); got != want {
						t.Errorf("header %s = %q, want %q", name, got, want)
					}
				}

				claims, key, err := verifyVAPID(r.Header.Get("Authorization"))
				if err != nil {
					t.Fatalf("VAPID: %v", err)
				}
				if key != f.VAPIDPublicKey() || key != f.keys.Public {
					t.Errorf("VAPID k = %q, want the server's public key %q", key, f.keys.Public)
				}
				if claims.Aud != wantAud {
					t.Errorf("aud = %q, want %q", claims.Aud, wantAud)
				}
				if claims.Sub != subject {
					t.Errorf("sub = %q, want %q", claims.Sub, subject)
				}
				exp := time.Unix(claims.Exp, 0)
				if d := exp.Sub(before); d < 12*time.Hour-time.Minute || d > 12*time.Hour+time.Minute {
					t.Errorf("exp is %v from now, want 12 h (and never over 24 h, RFC 8292)", d)
				}

				// The body is exactly one padded 4096-byte record, so its length says nothing about the payload.
				if len(r.Body) != 4096 {
					t.Errorf("body is %d bytes, want one 4096-byte record", len(r.Body))
				}
				plain, rs, err := ua.decrypt(r.Body)
				if err != nil {
					t.Fatalf("the subscription's key can't decrypt the message: %v", err)
				}
				if rs != 4096 {
					t.Errorf("record size %d, want 4096", rs)
				}
				if _, _, err := other.decrypt(r.Body); err == nil {
					t.Error("another browser's key decrypts the message")
				}
				if len(plain) >= maxPayloadBytes {
					t.Errorf("payload is %d bytes, want under 1 KB", len(plain))
				}
				var got api.PushPayload
				if err := json.Unmarshal(plain, &got); err != nil {
					t.Fatalf("payload %q: %v", plain, err)
				}
				want, _ := json.Marshal(wantPayload)
				if string(plain) != string(want) {
					t.Errorf("payload\n got %s\nwant %s", plain, want)
				}
			}

			at := time.UnixMilli(1790712000000)
			f.ShareStarted(ShareStarted{
				RoomID: "lounge", RoomName: "Lounge", ShareID: "s_q7m2x9c4v8b1n5k3",
				UserID: "k3m9p2qxw7ht", UserName: "Alex", PresentUserIDs: []string{"dave"}, At: at,
			})
			check("600", "high", shareTopic("lounge", "k3m9p2qxw7ht"), api.PushPayload{
				V: 1, Type: api.PushTypeShareStarted, TS: 1790712000000, Tag: "share:lounge:k3m9p2qxw7ht",
				URL:  "/r/lounge?focus=s_q7m2x9c4v8b1n5k3",
				Room: &api.NameRef{ID: "lounge", Name: "Lounge"}, User: &api.NameRef{ID: "k3m9p2qxw7ht", Name: "Alex"},
				ShareID: "s_q7m2x9c4v8b1n5k3",
			})

			f.AdminAlert(AdminAlert{Kind: "signup_pending", Actor: "system", Target: "sam_k", At: at})
			check("86400", "normal", "", api.PushPayload{
				V: 1, Type: api.PushTypeAdminAlert, TS: 1790712000000, Tag: "admin:signup_pending",
				URL: "/admin/approvals", Kind: api.AdminAlertKindSignupPending, Actor: "system", Target: "sam_k",
			})

			if err := f.SendTest(context.Background(), []Subscription{bob}); err != nil {
				t.Fatalf("SendTest: %v", err)
			}
			r := ps.next(t)
			plain, _, err := ua.decrypt(r.Body)
			if err != nil {
				t.Fatalf("push.test: %v", err)
			}
			var test api.PushPayload
			if err := json.Unmarshal(plain, &test); err != nil || test.Type != api.PushTypeTest || test.Tag != "test" ||
				test.URL != "/account/notifications" || test.V != 1 || test.TS == 0 {
				t.Errorf("push.test payload %s (%v)", plain, err)
			}
			if r.Header.Get("TTL") != "60" || r.Header.Get("Urgency") != "high" || r.Header.Get("Topic") != "" {
				t.Errorf("push.test headers TTL=%q Urgency=%q Topic=%q, want 60, high, none",
					r.Header.Get("TTL"), r.Header.Get("Urgency"), r.Header.Get("Topic"))
			}

			// 201 from the push service: the three deliveries are recorded as successes.
			waitFor(t, "three recorded results", func() bool { return len(f.store.snapshot().results) == 3 })
			for _, res := range f.store.snapshot().results {
				if res.ID != "sub1" || !res.OK {
					t.Errorf("recorded %+v, want a success for sub1", res)
				}
			}
			if f.sent(resultOK) != 3 {
				t.Errorf("ok = %d, want 3", f.sent(resultOK))
			}
			stop()
			if strings.Contains(f.logs.String(), "SECRET-CAPABILITY-TOKEN") {
				t.Errorf("the endpoint is in the log:\n%s", f.logs.String())
			}
		})
	}
}

// realSender returns the fixture's real sender.
func realSender(t testing.TB, f *fixture) *webPushSender {
	t.Helper()
	wp, ok := f.Service.sender.(*webPushSender)
	if !ok {
		t.Fatalf("the sender is %T, want the real one", f.Service.sender)
	}
	t.Cleanup(wp.Close)
	return wp
}

// The real guard, because DNS can change: the name was public when ValidateEndpoint looked, and resolves to a
// private address when the sender dials. The dialer's Control sees the real address and refuses it.
func TestSenderBlocksRebinding(t *testing.T) {
	// A listener the server must never reach: any connection to it fails the test.
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int32
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			_ = c.Close()
		}
	})
	defer wg.Wait()
	defer func() { _ = ln.Close() }()

	res := &fakeResolver{hosts: map[string][]string{"rebind.example.com": {publicV4}}}
	f := newFixture(t, func(o *Options) { o.Sender, o.Resolver = nil, res })
	wp := realSender(t, f)
	const endpoint = "https://rebind.example.com/send/SECRET-TOKEN"
	if err := f.ValidateEndpoint(context.Background(), endpoint); err != nil {
		t.Fatalf("ValidateEndpoint = %v, want nil while the name resolves to a public address", err)
	}

	guarded := wp.dial // the net.Dialer with the guard's Control
	ua := newUserAgent(t)
	targets := []string{
		ln.Addr().String(), // the listener above
		"127.0.0.1:443", "10.0.0.1:443", "192.168.1.1:443", "169.254.169.254:443", "100.64.0.1:443",
		"[::ffff:127.0.0.1]:443",
	}
	// Control runs once the socket exists, so the IPv6 targets need a host that can open IPv6 sockets.
	if probe, err := lc.Listen(context.Background(), "tcp6", "[::1]:0"); err == nil {
		_ = probe.Close()
		targets = append(targets, "[::1]:443", "[fd00::1]:443", "[64:ff9b::a00:1]:443")
	}
	for _, now := range targets {
		// What the dialer does after its own DNS lookup: connect to the address the name resolves to now.
		wp.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr != "rebind.example.com:443" {
				t.Errorf("dialing %q, want the endpoint's host and port", addr)
			}
			return guarded(ctx, network, now)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := wp.Send(ctx, ua.subscription("s1", "bob", endpoint), []byte("{}"), SendOptions{TTL: time.Minute})
		cancel()
		if !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("rebinding to %s: Send = %v, want ErrBlockedAddress", now, err)
		}
		if err != nil && strings.Contains(err.Error(), "SECRET-TOKEN") {
			t.Errorf("the error contains the endpoint: %v", err)
		}
		if classify(SendResult{}, err) != outcomeRefused {
			t.Errorf("rebinding to %s: the Service would retry", now)
		}
	}
	if n := accepted.Load(); n != 0 {
		t.Errorf("the private listener got %d connections, want none", n)
	}
}

// A push service that answers with a redirect is not followed: the Location would be a second URL that nothing
// checked.
func TestSenderFollowsNoRedirect(t *testing.T) {
	var followed atomic.Int32
	internal := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed.Add(1) }))
	defer internal.Close()

	for _, code := range []int{301, 302, 303, 307, 308} {
		ps := newPushService(t)
		ps.respond = func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/followed" {
				followed.Add(1)
				return
			}
			target := internal.URL + "/internal"
			if code == 307 {
				target = "/followed" // the same host is not followed either
			}
			http.Redirect(w, r, target, code)
		}
		f := newFixture(t, realSenderOptions(ps))
		wp := realSender(t, f)
		ua := newUserAgent(t)
		res, err := wp.Send(context.Background(), ua.subscription("s1", "bob", ps.srv.URL+"/send/x"), []byte("{}"),
			SendOptions{TTL: time.Minute, Urgency: UrgencyNormal})
		if err != nil || res.Status != code {
			t.Errorf("Send = %+v, %v; want the %d itself", res, err, code)
		}
		if classify(res, err) != outcomeRefused {
			t.Errorf("a %d is not counted as a refusal", code)
		}
		ps.next(t)
	}
	if n := followed.Load(); n != 0 {
		t.Errorf("%d redirects were followed, want none", n)
	}
}

// The guarded client of 04 §14.5: no proxy from the environment, no redirects, 10 s in total, and connections
// only through the guard's dialer.
func TestSenderClientSettings(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.Sender = nil })
	wp := realSender(t, f)
	if wp.transport.Proxy != nil {
		t.Error("the transport has a proxy function; the guard checks the dialed address, so there must be none")
	}
	if wp.client.Timeout != 10*time.Second {
		t.Errorf("timeout %v, want 10 s", wp.client.Timeout)
	}
	if err := wp.client.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Errorf("CheckRedirect = %v, want http.ErrUseLastResponse", err)
	}
	if wp.transport.DialContext == nil || wp.transport.DialTLSContext != nil {
		t.Error("the transport does not dial through the guarded dialer")
	}
	if wp.lax {
		t.Error("the real sender is lax without the tests' switch")
	}
	// Outside the tests the guard is on: a loopback endpoint never leaves the process.
	wp.dial = func(context.Context, string, string) (net.Conn, error) {
		t.Error("the sender dialed for an endpoint that breaks the URL rules")
		return nil, errors.New("unreachable")
	}
	ua := newUserAgent(t)
	for _, endpoint := range []string{
		"https://127.0.0.1/send/x", "https://10.0.0.1:443/x", "http://push.example.com/x",
		"https://push.example.com:8443/x", "https://user@push.example.com/x", "https://localhost/x", "::not a url",
	} {
		_, err := wp.Send(context.Background(), ua.subscription("s1", "bob", endpoint), []byte("{}"), SendOptions{})
		if !errors.Is(err, ErrUndeliverable) {
			t.Errorf("Send to %q = %v, want ErrUndeliverable", endpoint, err)
		}
	}
}

func TestSenderResults(t *testing.T) {
	ps := newPushService(t)
	f := newFixture(t, realSenderOptions(ps))
	wp := realSender(t, f)
	ua := newUserAgent(t)
	good := ua.subscription("s1", "bob", ps.srv.URL+"/send/SECRET-TOKEN")
	ctx := context.Background()

	t.Run("status and Retry-After", func(t *testing.T) {
		ps.respond = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
		}
		res, err := wp.Send(ctx, good, []byte("{}"), SendOptions{TTL: time.Minute})
		if err != nil || res != (SendResult{Status: 429, RetryAfter: 7 * time.Second}) {
			t.Errorf("Send = %+v, %v; want 429 with Retry-After 7 s", res, err)
		}
		ps.next(t)
	})

	t.Run("a large response body is not read to its end", func(t *testing.T) {
		ps.respond = func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusGone)
			_, _ = w.Write(make([]byte, 1<<20))
		}
		res, err := wp.Send(ctx, good, []byte("{}"), SendOptions{TTL: time.Minute})
		if err != nil || res.Status != http.StatusGone {
			t.Errorf("Send = %+v, %v; want 410", res, err)
		}
		ps.next(t)
	})

	t.Run("keys that can't be used", func(t *testing.T) {
		ps.respond = nil
		offCurve := make([]byte, 65)
		offCurve[0] = 4
		for name, s := range map[string]Subscription{
			"p256dh off the curve": {ID: "s", Endpoint: good.Endpoint, P256dh: b64(offCurve), Auth: good.Auth},
			"p256dh not base64":    {ID: "s", Endpoint: good.Endpoint, P256dh: "!!!", Auth: good.Auth},
			"auth not base64":      {ID: "s", Endpoint: good.Endpoint, P256dh: good.P256dh, Auth: "!!!"},
		} {
			_, err := wp.Send(ctx, s, []byte("{}"), SendOptions{TTL: time.Minute})
			if !errors.Is(err, ErrUndeliverable) || errors.Is(err, ErrBlockedAddress) {
				t.Errorf("%s: Send = %v, want ErrUndeliverable", name, err)
			}
		}
		select {
		case r := <-ps.got:
			t.Errorf("a request went out for unusable keys: %s %s", r.Method, r.Path)
		default:
		}
	})

	t.Run("a network error is retried and hides the endpoint", func(t *testing.T) {
		// A port where nothing listens: take one and close it.
		var lc net.ListenConfig
		ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		_, err = wp.Send(ctx, ua.subscription("s2", "bob", "https://"+addr+"/send/SECRET-TOKEN"), []byte("{}"),
			SendOptions{TTL: time.Minute})
		if err == nil || errors.Is(err, ErrUndeliverable) {
			t.Fatalf("Send = %v, want a network error that is retried", err)
		}
		if classify(SendResult{}, err) != outcomeTransient {
			t.Error("a refused connection is not retried")
		}
		if strings.Contains(err.Error(), "SECRET-TOKEN") || strings.Contains(err.Error(), "/send/") {
			t.Errorf("the error contains the endpoint: %v", err)
		}
	})

	t.Run("an untrusted certificate is an error", func(t *testing.T) {
		strict := newFixture(t, func(o *Options) { o.Sender, o.allowPrivate = nil, true }) // no rootCAs
		swp := realSender(t, strict)
		_, err := swp.Send(ctx, good, []byte("{}"), SendOptions{TTL: time.Minute})
		if err == nil {
			t.Fatal("Send to a server with an unknown certificate succeeded")
		}
		if strings.Contains(err.Error(), "SECRET-TOKEN") {
			t.Errorf("the error contains the endpoint: %v", err)
		}
	})
}

// With the guard on: the URL rules apply, and the request goes to the endpoint exactly as the browser gave it.
// Only an explicit default port is dropped, because the token's aud claim is the origin (RFC 8292 §2), and an
// origin has no default port.
func TestSenderKeepsTheEndpointAndTheOriginAsAudience(t *testing.T) {
	ps := newPushService(t)
	f := newFixture(t, func(o *Options) { o.Sender, o.rootCAs = nil, ps.roots() })
	wp := realSender(t, f)
	if wp.lax {
		t.Fatal("the guard is off")
	}
	// example.com (a name the test server's certificate is valid for) "resolves" to the fake push service.
	var mu sync.Mutex
	var dialed []string
	wp.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		dialed = append(dialed, addr)
		mu.Unlock()
		var d net.Dialer
		return d.DialContext(ctx, network, ps.srv.Listener.Addr().String())
	}
	ua := newUserAgent(t)
	//nolint:gosec // G101: made-up endpoints in the shapes of FCM's, WNS's and Mozilla's, not credentials
	for endpoint, wantURI := range map[string]string{
		"https://example.com/fcm/send/dx1:APA91b-x_y":             "/fcm/send/dx1:APA91b-x_y",
		"https://example.com:443/fcm/send/dx1:APA91b-x_y":         "/fcm/send/dx1:APA91b-x_y",
		"https://example.com:443/w/?token=AwYAAAC%2f8a%2b%3d":     "/w/?token=AwYAAAC%2f8a%2b%3d",
		"https://example.com/wpush/v2/gAAAA%2Fb%20c?x=1&y=%7E#f":  "/wpush/v2/gAAAA%2Fb%20c?x=1&y=%7E",
		"https://example.com:443/wpush/v2/gAAAA%2Fb%20c%7e?x=%7E": "/wpush/v2/gAAAA%2Fb%20c%7e?x=%7E",
		"HTTPS://EXAMPLE.COM:443/Send/ABC":                        "/Send/ABC",
	} {
		res, err := wp.Send(context.Background(), ua.subscription("s1", "bob", endpoint), []byte(`{"v":1}`),
			SendOptions{TTL: time.Minute})
		if err != nil || res.Status != http.StatusCreated {
			t.Fatalf("Send to %s = %+v, %v", endpoint, res, err)
		}
		r := ps.next(t)
		if r.URI != wantURI {
			t.Errorf("%s: request target %q, want %q", endpoint, r.URI, wantURI)
		}
		if r.Host != "example.com" {
			t.Errorf("%s: Host %q, want example.com", endpoint, r.Host)
		}
		claims, _, err := verifyVAPID(r.Header.Get("Authorization"))
		if err != nil || claims.Aud != "https://example.com" {
			t.Errorf("%s: aud %q (%v), want the origin https://example.com", endpoint, claims.Aud, err)
		}
		if plain, _, err := ua.decrypt(r.Body); err != nil || string(plain) != `{"v":1}` {
			t.Errorf("%s: payload %q, %v", endpoint, plain, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(dialed) == 0 {
		t.Fatal("nothing was dialed")
	}
	for _, addr := range dialed {
		if addr != "example.com:443" {
			t.Errorf("dialed %q, want example.com:443", addr)
		}
	}
}

func TestCanonicalEndpoint(t *testing.T) {
	for raw, want := range map[string]string{
		// What browsers give stays as it is, byte for byte.
		"https://fcm.googleapis.com/fcm/send/a%2Fb:c?x=%7e&y=A+B": "https://fcm.googleapis.com/fcm/send/a%2Fb:c?x=%7e&y=A+B",
		"https://127.0.0.1:49152/push/x":                          "https://127.0.0.1:49152/push/x",
		"https://[::1]:443/push/x":                                "https://[::1]:443/push/x",
		// Another spelling of the same origin is rewritten; the path and the query are not touched.
		"https://fcm.googleapis.com:443/fcm/send/a%2Fb?x=%7e": "https://fcm.googleapis.com/fcm/send/a%2Fb?x=%7e",
		"HTTPS://FCM.Googleapis.COM/Fcm/Send/ABC":             "https://fcm.googleapis.com/Fcm/Send/ABC",
		"https://Push.Example.com:8443/x":                     "https://push.example.com:8443/x",
	} {
		u, reason := parseEndpoint(raw, true)
		if reason != "" {
			t.Fatalf("parseEndpoint(%q) = %q", raw, reason)
		}
		if got := canonicalEndpoint(raw, u); got != want {
			t.Errorf("canonicalEndpoint(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	for v, want := range map[string]time.Duration{
		"":                              0,
		"7":                             7 * time.Second,
		" 30 ":                          30 * time.Second,
		"0":                             0,
		"-5":                            0,
		"soon":                          0,
		"1.5":                           0,
		"99999999999999999999":          0,
		"9223372036854775807":           24 * time.Hour,
		"Thu, 08 Oct 2026 12:00:45 GMT": 45 * time.Second,
		"Thu, 08 Oct 2026 11:59:00 GMT": 0,
		"Fri, 09 Oct 2026 12:00:00 GMT": 24 * time.Hour,
		"Sat, 10 Oct 2026 12:00:00 GMT": 24 * time.Hour,
	} {
		if got := parseRetryAfter(v, now); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", v, got, want)
		}
	}
}
