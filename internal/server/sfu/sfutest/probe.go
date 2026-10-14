package sfutest

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/pion/webrtc/v4"
)

// ProbeClient is the browser's side of a connection-test probe (02 §7.6, 05 §14.2): a Pion PeerConnection with one
// data channel, "probe", that offers, takes the SFU's answer (sfu.SFU.Probe, or 04's POST /api/v1/conntest) and
// exchanges messages with the probe PC, which echoes them. Like a browser it sends its offer without waiting for its
// candidates: the SFU learns its address from its connectivity checks (peer-reflexive).
type ProbeClient struct {
	pc        *webrtc.PeerConnection
	dc        *webrtc.DataChannel
	connected chan struct{} // closed when the PC first reached connected
	open      chan struct{} // closed when the data channel opened
	done      chan struct{} // closed when the data channel closed, or the PC failed or closed
	msgs      chan webrtc.DataChannelMessage

	connectedOnce, openOnce, doneOnce sync.Once
}

// probeClientQueue is how many messages a ProbeClient keeps that the test hasn't taken with Receive; Pion's read loop
// never waits for the test, so a message that finds the queue full is dropped.
const probeClientQueue = 256

// ErrProbeOver is Receive's and WaitOpen's error once the probe is over for the client (ProbeClient.Done).
var ErrProbeOver = errors.New("sfutest: the probe is over")

// NewProbeClient returns a ProbeClient on Pion settings se: LoopbackSettings for the UDP probe, LoopbackTCPSettings
// for the TCP ones (it dials the SFU's passive candidates).
func NewProbeClient(se webrtc.SettingEngine) (*ProbeClient, error) {
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, fmt.Errorf("sfutest: probe client PC: %w", err)
	}
	c := &ProbeClient{
		pc:        pc,
		connected: make(chan struct{}),
		open:      make(chan struct{}),
		done:      make(chan struct{}),
		msgs:      make(chan webrtc.DataChannelMessage, probeClientQueue),
	}
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		switch s {
		case webrtc.PeerConnectionStateConnected:
			c.connectedOnce.Do(func() { close(c.connected) })
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			c.over()
		default:
		}
	})
	dc, err := pc.CreateDataChannel("probe", nil)
	if err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("sfutest: probe data channel: %w", err)
	}
	c.dc = dc
	dc.OnOpen(func() { c.openOnce.Do(func() { close(c.open) }) })
	dc.OnClose(c.over)
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		select {
		case c.msgs <- m:
		default:
		}
	})
	return c, nil
}

func (c *ProbeClient) over() { c.doneOnce.Do(func() { close(c.done) }) }

// PC returns the client's PeerConnection.
func (c *ProbeClient) PC() *webrtc.PeerConnection { return c.pc }

// Offer returns the offer as a browser posts it (05 §14.2 step 2): set as the local description, with one data
// channel and no candidates.
func (c *ProbeClient) Offer() (string, error) {
	offer, err := c.pc.CreateOffer(nil)
	if err != nil {
		return "", fmt.Errorf("sfutest: probe offer: %w", err)
	}
	if err := c.pc.SetLocalDescription(offer); err != nil {
		return "", fmt.Errorf("sfutest: set the probe offer: %w", err)
	}
	return offer.SDP, nil
}

// Accept applies the SFU's answer.
func (c *ProbeClient) Accept(answer string) error {
	if err := c.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		return fmt.Errorf("sfutest: apply the probe answer: %w", err)
	}
	return nil
}

// WaitOpen waits until the PeerConnection is connected and the data channel open, the browser's test of success
// (05 §14.2 step 4). It returns ErrProbeOver when the probe ends first; a probe that was open once stays so for it.
func (c *ProbeClient) WaitOpen(ctx context.Context) error {
	for _, ch := range []chan struct{}{c.connected, c.open} {
		select {
		case <-ch:
			continue
		default:
		}
		select {
		case <-ch:
		case <-c.done:
			return ErrProbeOver
		case <-ctx.Done():
			return fmt.Errorf("sfutest: probe not open (PC %s): %w", c.pc.ConnectionState(), ctx.Err())
		}
	}
	return nil
}

// Send sends one message on the data channel: text when text is true, else binary.
func (c *ProbeClient) Send(text bool, data []byte) error {
	var err error
	if text {
		err = c.dc.SendText(string(data))
	} else {
		err = c.dc.Send(data)
	}
	if err != nil {
		return fmt.Errorf("sfutest: probe send: %w", err)
	}
	return nil
}

// Receive returns the next message that came back on the data channel. It returns ErrProbeOver when the probe is
// over and nothing is left to take.
func (c *ProbeClient) Receive(ctx context.Context) (webrtc.DataChannelMessage, error) {
	select {
	case m := <-c.msgs:
		return m, nil
	default:
	}
	select {
	case m := <-c.msgs:
		return m, nil
	case <-c.done:
		return webrtc.DataChannelMessage{}, ErrProbeOver
	case <-ctx.Done():
		return webrtc.DataChannelMessage{}, fmt.Errorf("sfutest: no probe echo: %w", ctx.Err())
	}
}

// Done is closed when the probe is over for the client: its data channel closed, or its PeerConnection failed or
// closed (the SFU closing the probe PC closes it, with DTLS's close_notify).
func (c *ProbeClient) Done() <-chan struct{} { return c.done }

// CloseChannel closes the data channel, which ends the probe on the SFU too.
func (c *ProbeClient) CloseChannel() error {
	if err := c.dc.Close(); err != nil {
		return fmt.Errorf("sfutest: close the probe channel: %w", err)
	}
	return nil
}

// Close closes the PeerConnection. It is idempotent.
func (c *ProbeClient) Close() error {
	if err := c.pc.Close(); err != nil {
		return fmt.Errorf("sfutest: close the probe client: %w", err)
	}
	return nil
}
