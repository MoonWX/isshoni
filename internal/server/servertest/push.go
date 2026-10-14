package servertest

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/netx"
	"github.com/MoonWX/isshoni/internal/server/push"
)

// PushHost is the host of a PushService's endpoints. Like TLSDomain it never resolves in DNS (RFC 6761); under the
// PushService's resolver it is a public address.
const PushHost = "push.isshoni.test"

// pushHostAddr is what PushHost resolves to under a PushService: a public unicast address, so the server's
// subscribe-time SSRF guard takes the endpoint (04 §14.5). Nothing ever connects to it: the PushService is the
// server's Sender, and it delivers nothing.
var pushHostAddr = netip.MustParseAddr("1.1.1.1")

// PushMessage is one message that the server sent through a PushService: to which subscription, what the browser's
// service worker would have decrypted, and the headers of RFC 8030.
type PushMessage struct {
	Endpoint  string // the subscription's endpoint, PushService.Endpoint(name)
	SubID     string // the subscription's id (03's push_subscriptions.id)
	UserID    string // the subscription's user
	SessionID string // the web session that subscribed
	// Payload is the message's JSON (04 §14.3), and Raw its bytes as the server would have encrypted them.
	Payload api.PushPayload
	Raw     []byte
	Options push.SendOptions // TTL, Urgency and Topic
}

// PushService is a fake Web Push service for a test server: the browsers' push services, as far as the server can
// tell. It is a push.Sender, which records every message instead of encrypting and delivering it, and a
// netx.Resolver, under which PushHost resolves to a public address and every other name as the system resolves it.
// A test passes it as both (04 §6.6, §14):
//
//	pushes := servertest.NewPushService()
//	srv := servertest.Start(t, servertest.Options{Deps: server.Deps{PushSender: pushes, Resolver: pushes}})
//
// The push service's real sender reaches only public addresses on port 443, and its guard has no switch outside
// its own package, so a test server can't send to a push service on a loopback port; this one stands in for it.
// The encryption and the VAPID token are push's own tests' (internal/server/push). A browser subscribes with
// Subscription's body, and the messages for it arrive with its Endpoint. Every message is answered 201 Created,
// unless SetStatus says otherwise for its endpoint.
//
// It is safe for concurrent use.
type PushService struct {
	mu       sync.Mutex
	messages []PushMessage
	status   map[string]int // by endpoint
	changed  chan struct{}  // closed and replaced at every message
}

// NewPushService returns a push service that has received nothing yet.
func NewPushService() *PushService {
	return &PushService{status: map[string]int{}, changed: make(chan struct{})}
}

// Endpoint returns the endpoint of the browser called name at this service: https://push.isshoni.test/send/<name>.
func (p *PushService) Endpoint(name string) string {
	return "https://" + PushHost + "/send/" + url.PathEscape(name)
}

// Subscription returns what the browser called name posts to POST /api/v1/push/subscriptions: its Endpoint and a
// fresh pair of keys, a P-256 public key as an uncompressed point and a 16-byte auth secret, in base64url as
// PushSubscription.toJSON() writes them.
func (p *PushService) Subscription(name string) api.PushSubscribeRequest {
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		panic("servertest: generating a P-256 key: " + err.Error()) // crypto/rand does not fail
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth) // never fails (crypto/rand)
	enc := base64.RawURLEncoding
	return api.PushSubscribeRequest{
		Endpoint: p.Endpoint(name),
		Keys:     api.PushKeys{P256dh: enc.EncodeToString(key.PublicKey().Bytes()), Auth: enc.EncodeToString(auth)},
	}
}

// SetStatus makes the service answer the messages to endpoint with an HTTP status from now on: 404 or 410 (the
// subscription is gone), 401 or 403 (another VAPID key), 429 and 5xx (try again), … (04 §14.5).
func (p *PushService) SetStatus(endpoint string, status int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status[endpoint] = status
}

// Send implements push.Sender: it records the message and answers with the endpoint's status. It never fails.
func (p *PushService) Send(
	_ context.Context, sub push.Subscription, payload []byte, o push.SendOptions,
) (push.SendResult, error) {
	m := PushMessage{
		Endpoint:  sub.Endpoint.Reveal(),
		SubID:     sub.ID,
		UserID:    sub.UserID,
		SessionID: sub.SessionID,
		Raw:       slices.Clone(payload),
		Options:   o,
	}
	_ = json.Unmarshal(payload, &m.Payload) // a payload that is no JSON shows as a zero Payload next to its Raw
	p.mu.Lock()
	defer p.mu.Unlock()
	p.messages = append(p.messages, m)
	close(p.changed)
	p.changed = make(chan struct{})
	status, ok := p.status[m.Endpoint]
	if !ok {
		status = http.StatusCreated
	}
	return push.SendResult{Status: status}, nil
}

// LookupNetIP implements netx.Resolver: PushHost (in any case, with or without the final dot) is a public IPv4
// address, and every other name is net.DefaultResolver's, so the server's other lookups are as without the fake.
func (p *PushService) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	if !strings.EqualFold(strings.TrimSuffix(host, "."), PushHost) {
		return net.DefaultResolver.LookupNetIP(ctx, network, host)
	}
	if network == "ip6" {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return []netip.Addr{pushHostAddr}, nil
}

// Messages returns the messages so far, in the order they arrived.
func (p *PushService) Messages() []PushMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.messages)
}

// Wait waits until cond holds for the messages so far and returns them, or returns ctx's error with the messages
// that had arrived by then. cond is called with the service's lock released, once at first and then after every
// message.
func (p *PushService) Wait(ctx context.Context, cond func([]PushMessage) bool) ([]PushMessage, error) {
	for {
		p.mu.Lock()
		got, changed := slices.Clone(p.messages), p.changed
		p.mu.Unlock()
		if cond(got) {
			return got, nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return got, ctx.Err()
		}
	}
}

// The interfaces a PushService stands in for.
var (
	_ push.Sender   = (*PushService)(nil)
	_ netx.Resolver = (*PushService)(nil)
)
