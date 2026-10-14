package servertest_test

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/push"
	"github.com/MoonWX/isshoni/internal/server/servertest"
)

// TestPushService: the fake push service records what a server sends, answers with the status a test set, gives
// its host a public address, and hands out what a browser would subscribe with.
func TestPushService(t *testing.T) {
	ctx := context.Background()
	p := servertest.NewPushService()

	if got := p.Endpoint("Sam's phone"); got != "https://push.isshoni.test/send/Sam%27s%20phone" {
		t.Errorf("Endpoint = %q", got)
	}
	sub := p.Subscription("laptop")
	point, err1 := base64.RawURLEncoding.DecodeString(sub.Keys.P256dh)
	secret, err2 := base64.RawURLEncoding.DecodeString(sub.Keys.Auth)
	if sub.Endpoint != p.Endpoint("laptop") || err1 != nil || err2 != nil || len(point) != 65 || point[0] != 4 ||
		len(secret) != 16 {
		t.Errorf("Subscription = %+v: a %d-byte point, a %d-byte secret", sub, len(point), len(secret))
	}
	if again := p.Subscription("laptop"); again.Keys == sub.Keys {
		t.Error("two subscriptions have the same keys")
	}

	for _, host := range []string{servertest.PushHost, "PUSH.isshoni.test."} {
		for _, network := range []string{"ip", "ip4"} {
			addrs, err := p.LookupNetIP(ctx, network, host)
			if err != nil || len(addrs) != 1 || !addrs[0].Is4() || addrs[0].IsPrivate() || addrs[0].IsLoopback() {
				t.Errorf("LookupNetIP(%s, %s) = %v, %v; want one public IPv4 address", network, host, addrs, err)
			}
		}
	}
	var dnsErr *net.DNSError
	if addrs, err := p.LookupNetIP(ctx, "ip6", servertest.PushHost); !errors.As(err, &dnsErr) || !dnsErr.IsNotFound {
		t.Errorf("LookupNetIP(ip6) = %v, %v; want not found", addrs, err)
	}
	// Every other name is the system's: a literal address resolves to itself without a lookup.
	if addrs, err := p.LookupNetIP(ctx, "ip", "127.0.0.1"); err != nil || len(addrs) != 1 || addrs[0].Unmap() != netip.MustParseAddr("127.0.0.1") {
		t.Errorf("LookupNetIP(127.0.0.1) = %v, %v", addrs, err)
	}

	gone := p.Endpoint("gone")
	p.SetStatus(gone, http.StatusGone)
	payload := []byte(`{"v":1,"type":"push.test","ts":1790712000000,"tag":"test","url":"/account/notifications"}`)
	opts := push.SendOptions{TTL: time.Minute, Urgency: push.UrgencyHigh}
	for _, endpoint := range []string{p.Endpoint("laptop"), gone} {
		res, err := p.Send(ctx, push.Subscription{ID: "sub1", UserID: "u1", SessionID: "s1", Endpoint: logx.Secret(endpoint)},
			payload, opts)
		want := http.StatusCreated
		if endpoint == gone {
			want = http.StatusGone
		}
		if err != nil || res != (push.SendResult{Status: want}) {
			t.Errorf("Send to %s = %+v, %v; want %d", endpoint, res, err, want)
		}
	}
	payload[0] = 'x' // the service keeps a copy
	waitCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	msgs, err := p.Wait(waitCtx, func(m []servertest.PushMessage) bool { return len(m) == 2 })
	if err != nil || len(msgs) != 2 {
		t.Fatalf("Wait = %d messages, %v", len(msgs), err)
	}
	m := msgs[0]
	if m.Endpoint != p.Endpoint("laptop") || m.SubID != "sub1" || m.UserID != "u1" || m.SessionID != "s1" ||
		m.Options != opts || m.Raw[0] != '{' || m.Payload.Type != api.PushTypeTest || m.Payload.URL != "/account/notifications" {
		t.Errorf("the first message = %+v", m)
	}

	// Wait gives up with its context, and returns what had arrived.
	short, cancelShort := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancelShort()
	if got, err := p.Wait(short, func(m []servertest.PushMessage) bool { return len(m) > 2 }); !errors.Is(err, context.DeadlineExceeded) || len(got) != 2 {
		t.Errorf("Wait for a third message = %d messages, %v", len(got), err)
	}
	// And it wakes for a message that arrives while it waits.
	done := make(chan error, 1)
	go func() {
		_, err := p.Wait(waitCtx, func(m []servertest.PushMessage) bool { return len(m) == 3 })
		done <- err
	}()
	if _, err := p.Send(ctx, push.Subscription{Endpoint: logx.Secret(p.Endpoint("late"))}, payload, opts); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Errorf("Wait for the third message: %v", err)
	}
	if n := len(p.Messages()); n != 3 {
		t.Errorf("Messages = %d, want 3", n)
	}
}
