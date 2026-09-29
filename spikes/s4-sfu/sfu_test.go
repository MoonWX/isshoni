package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

// ---------- helpers shared with munger_test.go ----------

type testCodec struct {
	pt   webrtc.PayloadType
	plid string
}

func h264Cap(plid string) webrtc.RTPCodecCapability {
	return webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
		SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=" + plid}
}

func toParams(cs []testCodec) []webrtc.RTPCodecParameters {
	out := make([]webrtc.RTPCodecParameters, len(cs))
	for i, c := range cs {
		out[i] = webrtc.RTPCodecParameters{RTPCodecCapability: h264Cap(c.plid), PayloadType: c.pt}
	}
	return out
}

// ---------- in-process SFU ----------

func startSFU(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSFU(webrtc.NewICEUDPMux(nil, conn), true)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(s.ServeWS))
	t.Cleanup(func() { srv.Close(); conn.Close() })
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// clientAPI is what a test client (publisher or subscriber) uses: loopback only,
// H.264 constrained baseline + Opus, simulcast header extensions.
func clientAPI(t *testing.T) *webrtc.API {
	t.Helper()
	m := &webrtc.MediaEngine{}
	fb := []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}, {Type: "ccm", Parameter: "fir"}}
	c := h264Cap("42e01f")
	c.RTCPFeedback = fb
	must(t, m.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: c, PayloadType: 96}, webrtc.RTPCodecTypeVideo))
	must(t, m.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: opusCapability, PayloadType: 111}, webrtc.RTPCodecTypeAudio))
	ir := &interceptor.Registry{}
	must(t, webrtc.RegisterDefaultInterceptors(m, ir))
	var se webrtc.SettingEngine
	se.SetIncludeLoopbackCandidate(true)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir), webrtc.WithSettingEngine(se))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// ---------- test client (signaling) ----------

type testClient struct {
	t      *testing.T
	api    *webrtc.API
	ws     *websocket.Conn
	ctx    context.Context
	id     string
	wmu    sync.Mutex
	joined chan struct{}

	mu      sync.Mutex
	state   roomState
	pub     *webrtc.PeerConnection
	sub     *webrtc.PeerConnection
	pending map[string][]webrtc.ICECandidateInit
	pubAns  chan string
	recv    map[string]*recvTrack // key: shareID + "/" + kind
}

func dial(t *testing.T, url, name string) *testClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ws, _, err := websocket.Dial(ctx, url+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	ws.SetReadLimit(4 << 20)
	c := &testClient{t: t, api: clientAPI(t), ws: ws, ctx: ctx, joined: make(chan struct{}),
		pending: map[string][]webrtc.ICECandidateInit{}, pubAns: make(chan string, 1), recv: map[string]*recvTrack{}}
	t.Cleanup(func() {
		c.mu.Lock()
		pub, sub := c.pub, c.sub
		c.mu.Unlock()
		if pub != nil {
			pub.Close()
		}
		if sub != nil {
			sub.Close()
		}
		ws.CloseNow()
		cancel()
	})
	go c.readLoop()
	c.send(message{Type: "join", Name: name})
	select {
	case <-c.joined:
	case <-time.After(5 * time.Second):
		t.Fatal("join timed out")
	}
	return c
}

func (c *testClient) send(m message) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = wsjson.Write(c.ctx, c.ws, m)
}

func (c *testClient) readLoop() {
	for {
		var m message
		if err := wsjson.Read(c.ctx, c.ws, &m); err != nil {
			return
		}
		switch m.Type {
		case "welcome":
			c.id = m.ID
			close(c.joined)
		case "state":
			c.mu.Lock()
			c.state = *m.State
			c.mu.Unlock()
		case "pub.answer":
			c.pubAns <- m.SDP
		case "sub.offer":
			c.onSubOffer(m.SDP)
		case "ice":
			c.addCandidate(m.PC, *m.Candidate)
		case "error":
			c.t.Logf("%s: server error: %s", c.id, m.Error)
		}
	}
}

func (c *testClient) newPC(name string) *webrtc.PeerConnection {
	pc, err := c.api.NewPeerConnection(webrtc.Configuration{})
	must(c.t, err)
	pc.OnICECandidate(func(cand *webrtc.ICECandidate) {
		if cand != nil {
			init := cand.ToJSON()
			c.send(message{Type: "ice", PC: name, Candidate: &init})
		}
	})
	return pc
}

func (c *testClient) addCandidate(name string, cand webrtc.ICECandidateInit) {
	c.mu.Lock()
	pc := c.pub
	if name == "sub" {
		pc = c.sub
	}
	if pc == nil || pc.RemoteDescription() == nil {
		c.pending[name] = append(c.pending[name], cand)
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	_ = pc.AddICECandidate(cand)
}

func (c *testClient) flush(name string, pc *webrtc.PeerConnection) {
	c.mu.Lock()
	list := c.pending[name]
	delete(c.pending, name)
	c.mu.Unlock()
	for _, cand := range list {
		_ = pc.AddICECandidate(cand)
	}
}

func (c *testClient) onSubOffer(sdpText string) {
	c.mu.Lock()
	pc := c.sub
	if pc == nil {
		pc = c.newPC("sub")
		pc.OnTrack(c.onTrack)
		c.sub = pc
	}
	c.mu.Unlock()
	must(c.t, pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdpText}))
	c.flush("sub", pc)
	ans, err := pc.CreateAnswer(nil)
	must(c.t, err)
	must(c.t, pc.SetLocalDescription(ans))
	c.send(message{Type: "sub.answer", SDP: ans.SDP})
}

func (c *testClient) subscribe(share, video string, audio bool) {
	c.send(message{Type: "subscribe", Share: share, Video: video, Audio: &audio})
}

// waitShare waits until peer ownerID's share is announced with all wanted layers.
func (c *testClient) waitShare(ownerID string, layers ...string) string {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		st := c.state
		c.mu.Unlock()
		for _, sh := range st.Shares {
			if sh.PeerID == ownerID && containsAll(sh.Layers, layers) {
				return sh.ID
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.t.Fatalf("share of %s with layers %v not announced", ownerID, layers)
	return ""
}

func containsAll(have, want []string) bool {
	for _, w := range want {
		found := false
		for _, h := range have {
			found = found || h == w
		}
		if !found {
			return false
		}
	}
	return true
}

// ---------- receiving side ----------

type recvPkt struct {
	seq   uint16
	ts    uint32
	nal   byte // NAL type (video) or 0
	layer byte // 'f' | 'q' (video) or 'a'
	at    time.Time
}

type recvTrack struct {
	mu   sync.Mutex
	pkts []recvPkt
}

func (r *recvTrack) snapshot() []recvPkt {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recvPkt(nil), r.pkts...)
}

func (c *testClient) track(share, kind string) *recvTrack {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := share + "/" + kind
	if c.recv[key] == nil {
		c.recv[key] = &recvTrack{}
	}
	return c.recv[key]
}

func (c *testClient) onTrack(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
	rt := c.track(tr.StreamID(), tr.Kind().String())
	go func() {
		for {
			pkt, _, err := tr.ReadRTP()
			if err != nil {
				return
			}
			p := recvPkt{seq: pkt.SequenceNumber, ts: pkt.Timestamp, at: time.Now(), layer: 'a'}
			if tr.Kind() == webrtc.RTPCodecTypeVideo && len(pkt.Payload) >= 2 {
				p.nal, p.layer = pkt.Payload[0]&0x1F, pkt.Payload[1]
			}
			rt.mu.Lock()
			rt.pkts = append(rt.pkts, p)
			rt.mu.Unlock()
		}
	}()
}

// ---------- publishing side ----------

// testTrack is a TrackLocal that stamps the mid/rid header extensions itself,
// which Pion's built-in tracks don't do for simulcast senders.
type testTrack struct {
	id, rid, stream string
	kind            webrtc.RTPCodecType
	codec           webrtc.RTPCodecCapability

	mu            sync.Mutex
	w             webrtc.TrackLocalWriter
	ssrc          webrtc.SSRC
	pt            webrtc.PayloadType
	midExt, ridEx uint8
	mid           string
}

func (tt *testTrack) Bind(ctx webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	codec, match := matchCodec(tt.codec, ctx.CodecParameters())
	if match == "none" {
		return webrtc.RTPCodecParameters{}, webrtc.ErrUnsupportedCodec
	}
	tt.mu.Lock()
	defer tt.mu.Unlock()
	tt.w, tt.ssrc, tt.pt = ctx.WriteStream(), ctx.SSRC(), codec.PayloadType
	for _, e := range ctx.HeaderExtensions() {
		switch e.URI {
		case sdp.SDESMidURI:
			tt.midExt = uint8(e.ID)
		case sdp.SDESRTPStreamIDURI:
			tt.ridEx = uint8(e.ID)
		}
	}
	return codec, nil
}
func (tt *testTrack) Unbind(webrtc.TrackLocalContext) error { return nil }
func (tt *testTrack) ID() string                            { return tt.id }
func (tt *testTrack) RID() string                           { return tt.rid }
func (tt *testTrack) StreamID() string                      { return tt.stream }
func (tt *testTrack) Kind() webrtc.RTPCodecType             { return tt.kind }

func (tt *testTrack) write(h rtp.Header, payload []byte) {
	tt.mu.Lock()
	w, ssrc, pt, midExt, ridExt, mid := tt.w, tt.ssrc, tt.pt, tt.midExt, tt.ridEx, tt.mid
	tt.mu.Unlock()
	if w == nil || mid == "" {
		return
	}
	h.Version, h.SSRC, h.PayloadType = 2, uint32(ssrc), uint8(pt)
	if midExt != 0 {
		_ = h.SetExtension(midExt, []byte(mid))
	}
	if ridExt != 0 && tt.rid != "" {
		_ = h.SetExtension(ridExt, []byte(tt.rid))
	}
	_, _ = w.WriteRTP(&h, payload)
}

type testPublisher struct {
	forceKey map[string]*atomic.Bool
	plis     map[string]*atomic.Int64
}

// publish starts a simulcast (f: 30 fps, q: 15 fps) synthetic H.264 + Opus share.
func (c *testClient) publish(ctx context.Context) *testPublisher {
	pc := c.newPC("pub")
	c.mu.Lock()
	c.pub = pc
	c.mu.Unlock()
	stream := "pub-" + c.id
	vf := &testTrack{id: "video", rid: "f", stream: stream, kind: webrtc.RTPCodecTypeVideo, codec: h264Cap("42e01f")}
	vq := &testTrack{id: "video", rid: "q", stream: stream, kind: webrtc.RTPCodecTypeVideo, codec: h264Cap("42e01f")}
	au := &testTrack{id: "audio", stream: stream, kind: webrtc.RTPCodecTypeAudio, codec: opusCapability}
	vs, err := pc.AddTrack(vf)
	must(c.t, err)
	must(c.t, vs.AddEncoding(vq))
	_, err = pc.AddTrack(au)
	must(c.t, err)

	offer, err := pc.CreateOffer(nil)
	must(c.t, err)
	must(c.t, pc.SetLocalDescription(offer))
	c.send(message{Type: "pub.offer", SDP: offer.SDP})
	select {
	case ans := <-c.pubAns:
		must(c.t, pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: ans}))
	case <-time.After(5 * time.Second):
		c.t.Fatal("no pub answer")
	}
	c.flush("pub", pc)
	for _, tr := range pc.GetTransceivers() {
		for _, tt := range []*testTrack{vf, vq, au} {
			if tr.Kind() == tt.kind {
				tt.mu.Lock()
				tt.mid = tr.Mid()
				tt.mu.Unlock()
			}
		}
	}

	p := &testPublisher{
		forceKey: map[string]*atomic.Bool{"f": {}, "q": {}},
		plis:     map[string]*atomic.Int64{"f": {}, "q": {}},
	}
	for _, rid := range []string{"f", "q"} {
		go func() {
			for {
				pkts, _, err := vs.ReadSimulcastRTCP(rid)
				if err != nil {
					return
				}
				for _, pkt := range pkts {
					if _, ok := pkt.(*rtcp.PictureLossIndication); ok {
						p.plis[rid].Add(1)
						p.forceKey[rid].Store(true)
					}
				}
			}
		}()
	}
	go runVideo(ctx, vf, 'f', 30, 60, p.forceKey["f"])
	go runVideo(ctx, vq, 'q', 15, 30, p.forceKey["q"])
	go runAudio(ctx, au)
	return p
}

// runVideo sends synthetic H.264 frames: keyframes are SPS, PPS, IDR packets;
// delta frames one non-IDR packet. Payload byte 1 identifies the layer.
func runVideo(ctx context.Context, tt *testTrack, layer byte, fps, gop int, forceKey *atomic.Bool) {
	tick := time.NewTicker(time.Second / time.Duration(fps))
	defer tick.Stop()
	seq, ts := uint16(rand.Uint32()), rand.Uint32()
	for frame := 0; ; frame++ {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		nal := func(typ byte, size int) []byte {
			b := make([]byte, size)
			b[0], b[1] = typ, layer
			binary.BigEndian.PutUint32(b[2:], uint32(frame))
			return b
		}
		var nalus [][]byte
		if frame%gop == 0 || forceKey.Swap(false) {
			nalus = [][]byte{nal(0x67, 16), nal(0x68, 8), nal(0x65, 900)}
		} else {
			nalus = [][]byte{nal(0x41, 400)}
		}
		for i, n := range nalus {
			tt.write(rtp.Header{SequenceNumber: seq, Timestamp: ts, Marker: i == len(nalus)-1}, n)
			seq++
		}
		ts += uint32(90000 / fps)
	}
}

func runAudio(ctx context.Context, tt *testTrack) {
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	seq, ts := uint16(rand.Uint32()), rand.Uint32()
	payload := make([]byte, 80)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		tt.write(rtp.Header{SequenceNumber: seq, Timestamp: ts}, payload)
		seq++
		ts += 960
	}
}

// ---------- assertions ----------

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func countLayer(pkts []recvPkt, from int, layer byte) (n int) {
	for _, p := range pkts[from:] {
		if p.layer == layer {
			n++
		}
	}
	return n
}

// checkStream asserts the subscriber saw one continuous stream: consecutive
// sequence numbers and non-decreasing timestamps across every layer switch.
func checkStream(t *testing.T, pkts []recvPkt) {
	t.Helper()
	for i := 1; i < len(pkts); i++ {
		if pkts[i].seq != pkts[i-1].seq+1 {
			t.Fatalf("sequence gap at %d: %d -> %d", i, pkts[i-1].seq, pkts[i].seq)
		}
		if int32(pkts[i].ts-pkts[i-1].ts) < 0 {
			t.Fatalf("timestamp backwards at %d", i)
		}
	}
}

// checkSwitchesStartOnSPS asserts that every change of layer starts with an SPS.
func checkSwitchesStartOnSPS(t *testing.T, pkts []recvPkt) {
	t.Helper()
	for i := 1; i < len(pkts); i++ {
		if pkts[i].layer != pkts[i-1].layer && pkts[i].nal != naluTypeSPS {
			t.Fatalf("layer switch at %d (%c -> %c) did not start with SPS (nal %d)", i, pkts[i-1].layer, pkts[i].layer, pkts[i].nal)
		}
	}
	if len(pkts) > 0 && pkts[0].nal != naluTypeSPS {
		t.Fatalf("stream did not start with SPS (nal %d)", pkts[0].nal)
	}
}

// ---------- tests ----------

func TestSimulcastLayerSwitching(t *testing.T) {
	url := startSFU(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	alice := dial(t, url, "alice")
	pubState := alice.publish(ctx)
	bob := dial(t, url, "bob")
	share := bob.waitShare(alice.id, "f", "q")

	bob.subscribe(share, "high", true)
	video, audio := bob.track(share, "video"), bob.track(share, "audio")
	waitFor(t, 5*time.Second, "high layer", func() bool { return countLayer(video.snapshot(), 0, 'f') >= 30 })
	waitFor(t, 2*time.Second, "audio", func() bool { return len(audio.snapshot()) >= 10 })

	// high -> low: must switch within ~one PLI round trip (not a whole 2 s GOP).
	mark := len(video.snapshot())
	t0 := time.Now()
	bob.subscribe(share, "low", true)
	waitFor(t, 3*time.Second, "low layer", func() bool { return countLayer(video.snapshot(), mark, 'q') >= 5 })
	pkts := video.snapshot()
	var firstQ time.Time
	for _, p := range pkts[mark:] {
		if p.layer == 'q' {
			firstQ = p.at
			break
		}
	}
	t.Logf("high->low switch took %v (publisher PLIs: f=%d q=%d)", firstQ.Sub(t0),
		pubState.plis["f"].Load(), pubState.plis["q"].Load())
	if firstQ.Sub(t0) > time.Second {
		t.Errorf("switch took %v, want < 1s", firstQ.Sub(t0))
	}

	// low -> off -> high.
	bob.subscribe(share, "off", false)
	time.Sleep(300 * time.Millisecond)
	paused := len(video.snapshot())
	audioPaused := len(audio.snapshot())
	time.Sleep(300 * time.Millisecond)
	if n := len(video.snapshot()); n != paused {
		t.Errorf("video kept flowing while off: %d -> %d packets", paused, n)
	}
	if n := len(audio.snapshot()); n != audioPaused {
		t.Errorf("audio kept flowing while off: %d -> %d packets", audioPaused, n)
	}
	bob.subscribe(share, "high", true)
	waitFor(t, 3*time.Second, "resume high", func() bool { return countLayer(video.snapshot(), paused, 'f') >= 10 })

	all := video.snapshot()
	checkStream(t, all)
	checkSwitchesStartOnSPS(t, all)
	checkStream(t, audio.snapshot())
}

func TestTenPublishersTenSubscribers(t *testing.T) {
	if testing.Short() {
		t.Skip("load test")
	}
	const n = 10
	url := startSFU(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pubs := make([]*testClient, n)
	for i := range pubs {
		pubs[i] = dial(t, url, fmt.Sprintf("pub%d", i))
		pubs[i].publish(ctx)
	}
	subs := make([]*testClient, n)
	shares := make([]string, n)
	start := time.Now()
	for i := range subs {
		subs[i] = dial(t, url, fmt.Sprintf("sub%d", i))
	}
	for j := range pubs {
		shares[j] = subs[0].waitShare(pubs[j].id, "f", "q")
	}
	// Each viewer focuses a different share (high + audio); the rest are thumbnails.
	for i, s := range subs {
		for j, share := range shares {
			if i == j {
				s.subscribe(share, "high", true)
			} else {
				s.subscribe(share, "low", false)
			}
		}
	}
	for i, s := range subs {
		for j, share := range shares {
			want := byte('q')
			if i == j {
				want = 'f'
			}
			tr := s.track(share, "video")
			waitFor(t, 15*time.Second, fmt.Sprintf("sub%d/share%d layer %c", i, j, want),
				func() bool { return countLayer(tr.snapshot(), 0, want) >= 20 })
		}
	}
	t.Logf("%d×%d: every subscription received its layer after %v", n, n, time.Since(start))

	time.Sleep(time.Second) // let streams run, then verify integrity everywhere
	for i, s := range subs {
		for j, share := range shares {
			pkts := s.track(share, "video").snapshot()
			checkStream(t, pkts)
			checkSwitchesStartOnSPS(t, pkts)
			other := byte('f')
			if i == j {
				other = 'q'
			}
			if c := countLayer(pkts, 0, other); c != 0 {
				t.Errorf("sub%d got %d packets of unwanted layer %c from share%d", i, c, other, j)
			}
			audio := len(s.track(share, "audio").snapshot())
			if (i == j) != (audio > 0) {
				t.Errorf("sub%d share%d: audio packets %d (focused=%v)", i, j, audio, i == j)
			}
		}
	}
}
