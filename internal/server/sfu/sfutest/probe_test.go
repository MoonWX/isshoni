package sfutest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// TestProbeClient runs a ProbeClient against a plain Pion peer that echoes like the SFU's probe PC, without the SFU
// (integration 18, in package sfu, runs it against SFU.Probe): the offer has one data channel and no candidates,
// messages come back as they went, and Done, Receive and WaitOpen tell when the peer has closed the probe.
func TestProbeClient(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	c, err := NewProbeClient(LoopbackSettings())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	offer, err := c.Offer()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(offer, "m=application") || strings.Contains(offer, "m=audio") ||
		strings.Contains(offer, "m=video") || strings.Contains(offer, "a=candidate:") {
		t.Fatalf("the offer is not one data channel without candidates:\n%s", offer)
	}

	// The echo peer: it answers with all its candidates, as the SFU does, and sends back what it gets.
	peer, err := webrtc.NewAPI(webrtc.WithSettingEngine(LoopbackSettings())).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	peer.OnDataChannel(func(dc *webrtc.DataChannel) {
		dc.OnMessage(func(m webrtc.DataChannelMessage) {
			if m.IsString {
				_ = dc.SendText(string(m.Data))
			} else {
				_ = dc.Send(m.Data)
			}
		})
	})
	if err := peer.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		t.Fatal(err)
	}
	answer, err := peer.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(peer)
	if err := peer.SetLocalDescription(answer); err != nil {
		t.Fatal(err)
	}
	<-gathered
	if err := c.Accept(peer.LocalDescription().SDP); err != nil {
		t.Fatal(err)
	}
	if err := c.WaitOpen(ctx); err != nil {
		t.Fatal(err)
	}
	if c.PC().ConnectionState() != webrtc.PeerConnectionStateConnected {
		t.Errorf("open, but the PC is %s", c.PC().ConnectionState())
	}

	for _, m := range []webrtc.DataChannelMessage{
		{IsString: true, Data: []byte(`{"n":0,"t":1.5}`)},
		{IsString: false, Data: []byte{0, 0xff, 7}},
	} {
		if err := c.Send(m.IsString, m.Data); err != nil {
			t.Fatal(err)
		}
		got, err := c.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got.IsString != m.IsString || string(got.Data) != string(m.Data) {
			t.Errorf("echo %v %q, want %v %q", got.IsString, got.Data, m.IsString, m.Data)
		}
	}

	// The peer ends the probe: the client sees it, and has nothing more to receive.
	if err := peer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.Done():
	case <-ctx.Done():
		t.Fatalf("the client's probe is not over after the peer closed (PC %s)", c.PC().ConnectionState())
	}
	if _, err := c.Receive(ctx); !errors.Is(err, ErrProbeOver) {
		t.Errorf("Receive after the end = %v, want ErrProbeOver", err)
	}
	if err := c.WaitOpen(ctx); err != nil {
		t.Errorf("WaitOpen after it was open = %v", err)
	}
}

// TestProbeClientNeverOpen: WaitOpen gives up with the context, and says ErrProbeOver once the client is closed.
func TestProbeClientNeverOpen(t *testing.T) {
	c, err := NewProbeClient(LoopbackTCPSettings())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := c.WaitOpen(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("WaitOpen without an answer = %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done not closed after Close")
	}
	if err := c.WaitOpen(t.Context()); !errors.Is(err, ErrProbeOver) {
		t.Errorf("WaitOpen after Close = %v, want ErrProbeOver", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("a second Close: %v", err)
	}
}
