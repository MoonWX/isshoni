package sfutest

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/server/netx"
	"github.com/MoonWX/isshoni/internal/server/sfu"
)

// TestHarness: a Harness is a real SFU on a loopback Transport with the transports its options ask for, and Close
// releases everything, twice if asked. (The SFU's own tests in internal/server/sfu negotiate through it.)
func TestHarness(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts HarnessOptions
		vias []string
	}{
		{"udp", HarnessOptions{}, []string{netx.ViaUDP}},
		{"all", HarnessOptions{TCP: true, TCP443: true}, []string{netx.ViaTCP443, netx.ViaTCP7882, netx.ViaUDP}},
		{"tcp443 only", HarnessOptions{NoUDP: true, TCP443: true}, []string{netx.ViaTCP443}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := NewHarness(context.Background(), tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			var vias []string
			for _, a := range h.Transport.Advertised {
				if a.Addr.Addr().String() != "127.0.0.1" {
					t.Errorf("advertised %v is not on loopback", a.Addr)
				}
				vias = append(vias, a.Via)
			}
			slices.Sort(vias)
			if !slices.Equal(vias, tc.vias) {
				t.Errorf("advertised transports %v, want %v", vias, tc.vias)
			}
			if (h.PortMux != nil) != tc.opts.TCP443 || (h.TCP443Port() != 0) != tc.opts.TCP443 {
				t.Errorf("PortMux %v, port %d with TCP443 = %v", h.PortMux != nil, h.TCP443Port(), tc.opts.TCP443)
			}
			if err := h.SFU.Ready(); err != nil {
				t.Errorf("Ready: %v", err)
			}

			conn, sig, err := h.Join(sfu.JoinParams{Room: "lounge", Participant: "u1", User: "u1", Conn: "c1", Role: sfu.RoleFull})
			if err != nil || conn == nil || sig == nil {
				t.Fatalf("Join = %v, %v, %v", conn, sig, err)
			}
			if _, _, err := h.Join(sfu.JoinParams{Room: "lounge", Participant: "u1", User: "u1", Conn: "c1", Role: sfu.RoleFull}); err == nil {
				t.Error("joining the same connection twice succeeded")
			}
			if _, err := conn.StartShare(context.Background(), sfu.StartShareParams{ID: "s_1"}); err != nil {
				t.Fatal(err)
			}

			if err := h.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
			if err := h.Close(); err != nil {
				t.Errorf("second Close: %v", err)
			}
			select {
			case <-conn.Done():
			case <-time.After(5 * time.Second):
				t.Error("the Conn is not done after Harness.Close")
			}
			evs := h.Events.Events()
			if len(evs) != 1 || evs[0].Kind != ShareEnded || evs[0].Share.ID != "s_1" ||
				evs[0].Reason != sfu.EndReasonServerShutdown || evs[0].Room != "lounge" {
				t.Errorf("room events = %+v, want the share ended by the shutdown", evs)
			}
		})
	}
}

// TestDirectSignaler: it records offers and events in order, its waits find them or give up with the context, and
// Close works with or without a Viewer.
func TestDirectSignaler(t *testing.T) {
	d := NewDirectSignaler()
	tracks := []sfu.TrackBinding{{MID: "0", Share: "s_1", Kind: webrtc.RTPCodecTypeVideo}}
	d.SendOffer(sfu.PCSub, 1, 1, "sdp-1", tracks)
	tracks[0].MID = "changed" // the Signaler keeps its own copy
	d.SendOffer(sfu.PCSub, 1, 2, "sdp-2", nil)
	d.SendEvent(sfu.PCStateEvent{PC: sfu.PCSub, Gen: 1, State: "connected"})
	d.SendEvent(sfu.CodecPolicyEvent{Share: "s_1", Profile: sfu.ProfileHigh})

	offers := d.Offers()
	if len(offers) != 2 || offers[0].SDP != "sdp-1" || offers[0].Tracks[0].MID != "0" || offers[1].Neg != 2 {
		t.Errorf("Offers = %+v", offers)
	}
	if evs := d.Events(); len(evs) != 2 {
		t.Errorf("Events = %+v", evs)
	}
	ctx := context.Background()
	if o, err := d.WaitOffer(ctx, func(o Offer) bool { return o.Neg == 2 }); err != nil || o.SDP != "sdp-2" {
		t.Errorf("WaitOffer = %+v, %v", o, err)
	}
	if err := d.WaitPCState(ctx, sfu.PCSub, 1, "connected"); err != nil {
		t.Errorf("WaitPCState: %v", err)
	}
	ev, err := d.WaitEvent(ctx, func(ev sfu.Event) bool { _, ok := ev.(sfu.CodecPolicyEvent); return ok })
	if err != nil || ev.(sfu.CodecPolicyEvent).Share != "s_1" {
		t.Errorf("WaitEvent = %+v, %v", ev, err)
	}

	short, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if _, err := d.WaitOffer(short, func(o Offer) bool { return o.Neg == 3 }); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("WaitOffer for a missing offer: %v", err)
	}
	if err := d.WaitPCState(short, sfu.PCPub, 1, "connected"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("WaitPCState for a missing state: %v", err)
	}
	if len(d.Errs()) != 0 {
		t.Errorf("Errs = %v", d.Errs())
	}
	d.Close() // never attached
	d.Close()
	d.Attach(nil, nil) // after Close: nothing starts
	d.SendOffer(sfu.PCSub, 1, 3, "sdp-3", nil)
	if n := len(d.Offers()); n != 3 {
		t.Errorf("%d offers after Close, want recording to go on", n)
	}
}

// TestRoomEventLog: every RoomEvents call is recorded, in order.
func TestRoomEventLog(t *testing.T) {
	l := &RoomEventLog{}
	l.ShareUpdated("r", sfu.ShareInfo{ID: "s_1", State: sfu.ShareLive})
	l.CodecPolicyChanged("r", sfu.ProfileConstrainedBaseline)
	l.ShareEnded("r", sfu.ShareInfo{ID: "s_1"}, sfu.EndReasonStopped)
	evs := l.Events()
	if len(evs) != 3 || evs[0].Kind != ShareUpdated || evs[0].Share.State != sfu.ShareLive ||
		evs[1].Kind != CodecPolicyChanged || evs[1].Policy != sfu.ProfileConstrainedBaseline ||
		evs[2].Kind != ShareEnded || evs[2].Reason != sfu.EndReasonStopped || evs[2].Room != "r" {
		t.Errorf("Events = %+v", evs)
	}
	ev, err := l.Wait(context.Background(), func(ev RoomEvent) bool { return ev.Kind == ShareEnded })
	if err != nil || ev.Share.ID != "s_1" {
		t.Errorf("Wait = %+v, %v", ev, err)
	}
	short, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := l.Wait(short, func(RoomEvent) bool { return false }); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Wait for nothing: %v", err)
	}
}
