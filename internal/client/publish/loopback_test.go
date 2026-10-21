package publish

import (
	"context"
	"encoding/binary"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/nack"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/media"
)

// receiver is the far end of a Publisher's PeerConnection: a Pion PeerConnection without interceptors, so that it
// asks for no repair and sends no report on its own, that takes H.264 6400 (with NACK feedback, so the Publisher's
// responder keeps the stream), and records what arrives. Its socket loses one chosen RTP packet (lossyConn), so that
// a NACK asks for a packet that really is missing. Its SRTP replay window is NACKResponderSize packets: a plain
// retransmission reuses the lost packet's SRTP index, which Pion's default window of 64 rejects once 64 newer packets
// of the stream arrived. Every receiver of a native publisher's repairs needs a window that large, the SFU's pub PCs
// included (D7: no RTX, so a repair is the packet again with its own sequence number).
type receiver struct {
	pc *webrtc.PeerConnection

	mu   sync.Mutex
	seqs []uint16 // video, in arrival order
	ssrc uint32
	srs  []srArrival
}

// srArrival is a sender report the receiver got, and when.
type srArrival struct {
	sr *rtcp.SenderReport
	at time.Time
}

// lossyConn is the receiver's UDP socket: it loses the first RTP datagram with sequence number drop, as a network
// would. The RTP header of an SRTP packet is in the clear.
type lossyConn struct {
	net.PacketConn
	drop    uint16
	dropped atomic.Bool
}

func (c *lossyConn) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		n, addr, err := c.PacketConn.ReadFrom(b)
		// RTP, not RTCP (RFC 5761 §4: RTCP packet types 192–223 in the second byte), with that sequence number.
		rtpPkt := err == nil && n >= 12 && b[0]>>6 == 2 && (b[1] < 192 || b[1] > 223)
		if rtpPkt && binary.BigEndian.Uint16(b[2:4]) == c.drop && c.dropped.CompareAndSwap(false, true) {
			continue
		}
		return n, addr, err
	}
}

// newReceiver returns a receiver whose socket loses the RTP packet with sequence number drop.
func newReceiver(t *testing.T, drop uint16) (*receiver, *lossyConn) {
	t.Helper()
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	conn := &lossyConn{PacketConn: udp, drop: drop}
	mux := webrtc.NewICEUDPMux(nil, conn)
	t.Cleanup(func() { _ = mux.Close() })
	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeH264, ClockRate: videoClock, SDPFmtpLine: h264FmtpPrefix + DefaultProfile + "1f",
			RTCPFeedback: []webrtc.RTCPFeedback{{Type: webrtc.TypeRTCPFBNACK}, {Type: webrtc.TypeRTCPFBNACK, Parameter: "pli"}},
		},
		PayloadType: videoPT,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		t.Fatal(err)
	}
	if err := m.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: sdp.SDESMidURI}, webrtc.RTPCodecTypeVideo); err != nil {
		t.Fatal(err)
	}
	se := loopbackSettings()
	se.SetICEUDPMux(mux)
	se.SetSRTPReplayProtectionWindow(NACKResponderSize)
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(se),
		webrtc.WithInterceptorRegistry(&interceptor.Registry{})).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	r := &receiver{pc: pc}
	t.Cleanup(func() { _ = pc.Close() })
	pc.OnTrack(func(tr *webrtc.TrackRemote, rr *webrtc.RTPReceiver) {
		r.mu.Lock()
		r.ssrc = uint32(tr.SSRC())
		r.mu.Unlock()
		go func() {
			for {
				pkt, _, err := tr.ReadRTP()
				if err != nil {
					return
				}
				r.mu.Lock()
				r.seqs = append(r.seqs, pkt.SequenceNumber)
				r.mu.Unlock()
			}
		}()
		go func() {
			for {
				pkts, _, err := rr.ReadRTCP()
				if err != nil {
					return
				}
				for _, p := range pkts {
					if sr, ok := p.(*rtcp.SenderReport); ok {
						r.mu.Lock()
						r.srs = append(r.srs, srArrival{sr: sr, at: time.Now()})
						r.mu.Unlock()
					}
				}
			}
		}()
	})
	return r, conn
}

// connect negotiates the Publisher with the receiver, the Publisher offering, and starts it.
func (r *receiver) connect(ctx context.Context, t *testing.T, p *Publisher) {
	t.Helper()
	offer, err := p.Offer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.pc.SetRemoteDescription(offer); err != nil {
		t.Fatal(err)
	}
	answer, err := r.pc.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(r.pc)
	if err := r.pc.SetLocalDescription(answer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gathered:
	case <-ctx.Done():
		t.Fatal("the receiver's candidates were not gathered in time")
	}
	if err := p.SetAnswer(*r.pc.LocalDescription()); err != nil {
		t.Fatal(err)
	}
	if err := p.Start(ctx); err != nil {
		t.Fatal(err)
	}
}

// arrived returns the video sequence numbers that arrived, in order, and the stream's SSRC.
func (r *receiver) arrived() ([]uint16, uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]uint16(nil), r.seqs...), r.ssrc
}

// burstSource hands out n small video frames of layer f, one RTP packet each, as fast as the receiver's socket takes
// them, then blocks. Their capture times are 1 ms apart.
type burstSource struct {
	n int

	mu   sync.Mutex
	sent int
}

func (s *burstSource) Next(ctx context.Context) (media.Packet, error) {
	s.mu.Lock()
	i := s.sent
	if i < s.n {
		s.sent++
	}
	s.mu.Unlock()
	if i == s.n {
		<-ctx.Done()
		return media.Packet{}, ctx.Err()
	}
	if i%100 == 99 {
		time.Sleep(2 * time.Millisecond) // keeps a burst within the socket buffers
	}
	var idc byte
	if i == 0 {
		idc = 0x64
	}
	return media.Packet{
		Kind: media.KindVideo, Layer: "f", Keyframe: i == 0, CaptureNS: int64(i) * int64(time.Millisecond),
		Data: annexB(200, i == 0, idc),
	}, nil
}

func (s *burstSource) RequestKeyframe(string) {}

// waitFor polls cond until it holds or the time is up.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(d); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestNACKAndRTT runs a Publisher against a real PeerConnection: the receiver's socket loses the packet 3000 before
// the newest, and a NACK for it is answered with that packet again, which Pion's NACK responder can only do with
// NACKResponderSize (4096) packets kept (D7, §8.4; Pion's default of 1024 couldn't), and which gets through an SRTP
// replay window of that size; and a receiver report crafted from a sender report the receiver got, with the delay
// it held it as DLSR, gives the Publisher a loopback RTT (§8.7). With and without pacing.
func TestNACKAndRTT(t *testing.T) {
	for _, pacing := range []bool{false, true} {
		name := "unpaced"
		if pacing {
			name = "paced"
		}
		t.Run(name, func(t *testing.T) {
			const n, back = 3100, 3000
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			p, err := New(Options{Source: &burstSource{n: n}, Layers: []string{"f"}, Settings: loopbackSettings(), Pacing: pacing})
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			// Every frame is one RTP packet, so the last one has sequence number base + n − 1; the receiver loses
			// the one 3000 before it. The send loop hasn't started: the base is safe to read.
			target := p.video[0].seq + n - 1 - back
			r, conn := newReceiver(t, target)
			r.connect(ctx, t, p)

			// Once the burst is written and nothing more arrives for 200 ms, the newest arrival is the last packet
			// (a loopback socket may lose a few: the responder keeps packets by sequence number, so a gap changes
			// nothing).
			waitFor(t, 10*time.Second, "the burst", func() bool { return p.Stats().Tracks[0].Packets == n })
			last, still := -1, time.Now()
			waitFor(t, 5*time.Second, "the burst to arrive", func() bool {
				seqs, _ := r.arrived()
				if len(seqs) != last {
					last, still = len(seqs), time.Now()
				}
				return time.Since(still) > 200*time.Millisecond
			})
			seqs, ssrc := r.arrived()
			if len(seqs) < n/2 {
				t.Fatalf("%d of %d packets arrived", len(seqs), n)
			}
			if !conn.dropped.Load() || slices.Contains(seqs, target) {
				t.Fatalf("the packet %d back was not lost (lost: %v)", back, conn.dropped.Load())
			}
			if newest := seqs[len(seqs)-1]; newest-target < back-100 {
				t.Fatalf("the newest packet that arrived is only %d after the lost one", newest-target)
			}
			before := len(seqs)
			nack := &rtcp.TransportLayerNack{SenderSSRC: 1, MediaSSRC: ssrc, Nacks: []rtcp.NackPair{{PacketID: target}}}
			if err := r.pc.WriteRTCP([]rtcp.Packet{nack}); err != nil {
				t.Fatal(err)
			}
			waitFor(t, 3*time.Second, "the retransmission of the packet 3000 back", func() bool {
				seqs, _ := r.arrived()
				for _, s := range seqs[min(before, len(seqs)):] {
					if s == target {
						return true
					}
				}
				return false
			})
			if st := p.Stats().Tracks[0]; st.NACKs != 1 {
				t.Errorf("NACKs counted = %d, want 1", st.NACKs)
			}

			// A sender report arrives within SRInterval; the receiver holds it 50 ms, then reports it.
			var got srArrival
			waitFor(t, 3*SRInterval, "a sender report", func() bool {
				r.mu.Lock()
				defer r.mu.Unlock()
				if len(r.srs) > 0 {
					got = r.srs[len(r.srs)-1]
				}
				return got.sr != nil
			})
			time.Sleep(50 * time.Millisecond)
			held := time.Since(got.at)
			rr := &rtcp.ReceiverReport{SSRC: 1, Reports: []rtcp.ReceptionReport{{
				SSRC: ssrc, LastSenderReport: uint32(got.sr.NTPTime >> 16), Delay: uint32(held * 65536 / time.Second),
			}}}
			if err := r.pc.WriteRTCP([]rtcp.Packet{rr}); err != nil {
				t.Fatal(err)
			}
			waitFor(t, 3*time.Second, "an RTT", func() bool { return p.Stats().Tracks[0].RTT > 0 })
			if rtt := p.Stats().Tracks[0].RTT; rtt > 100*time.Millisecond {
				t.Errorf("loopback RTT = %v, want well under 100 ms", rtt)
			} else {
				t.Logf("loopback RTT %v", rtt)
			}
			if p.Stats().Dropped != 0 {
				t.Errorf("Dropped = %d", p.Stats().Dropped)
			}
		})
	}
}

// TestResponderSize: the responder the Publisher uses keeps 4096 packets: it answers a NACK for the packet 3000 back,
// which Pion's default responder no longer has.
func TestResponderSize(t *testing.T) {
	for _, tc := range []struct {
		name string
		size func() (interceptor.Factory, error)
		want bool
	}{
		{"the Publisher's", func() (interceptor.Factory, error) { return newNACKResponder() }, true},
		{"Pion's default", func() (interceptor.Factory, error) { return nack.NewResponderInterceptor() }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := tc.size()
			if err != nil {
				t.Fatal(err)
			}
			i, err := f.NewInterceptor("")
			if err != nil {
				t.Fatal(err)
			}
			defer i.Close()
			resent := make(chan uint16, 1)
			var armed atomic.Bool // set once the stream's own packets are written: what comes then is a repair
			info := &interceptor.StreamInfo{SSRC: 5, RTCPFeedback: []interceptor.RTCPFeedback{{Type: "nack"}}}
			w := i.BindLocalStream(info, interceptor.RTPWriterFunc(
				func(h *rtp.Header, _ []byte, _ interceptor.Attributes) (int, error) {
					if armed.Load() {
						resent <- h.SequenceNumber
					}
					return 0, nil
				}))
			const newest = 3500
			for seq := uint16(0); seq <= newest; seq++ {
				if _, err := w.Write(&rtp.Header{SSRC: 5, SequenceNumber: seq}, []byte{1}, nil); err != nil {
					t.Fatal(err)
				}
			}
			armed.Store(true)
			raw, err := (&rtcp.TransportLayerNack{MediaSSRC: 5, Nacks: []rtcp.NackPair{{PacketID: newest - 3000}}}).Marshal()
			if err != nil {
				t.Fatal(err)
			}
			r := i.BindRTCPReader(interceptor.RTCPReaderFunc(
				func(b []byte, a interceptor.Attributes) (int, interceptor.Attributes, error) {
					return copy(b, raw), a, nil
				}))
			if _, _, err := r.Read(make([]byte, 1500), nil); err != nil {
				t.Fatal(err)
			}
			select {
			case seq := <-resent:
				if !tc.want || seq != newest-3000 {
					t.Errorf("resent %d", seq)
				}
			case <-time.After(300 * time.Millisecond):
				if tc.want {
					t.Error("the packet 3000 back was not resent")
				}
			}
		})
	}
}
