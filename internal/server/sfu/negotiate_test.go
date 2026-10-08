package sfu_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/client/publish"
	"github.com/MoonWX/isshoni/internal/media/fake"
	"github.com/MoonWX/isshoni/internal/server/netx"
	"github.com/MoonWX/isshoni/internal/server/sfu"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
)

// The core slice's acceptance tests (README S29), through sfutest.Harness: a real SFU on loopback, a
// publish.Publisher with fake media and an sfutest.Viewer negotiate the pub and sub PCs with gen, neg and tracks.
// Media forwarding is the next slice's (S41), so these tests stop at connected PCs, attached layers and bound
// DownTracks.

func newHarness(t *testing.T, o sfutest.HarnessOptions) *sfutest.Harness {
	t.Helper()
	h, err := sfutest.NewHarness(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Errorf("Harness.Close: %v", err)
		}
	})
	return h
}

func join(t *testing.T, h *sfutest.Harness, user sfu.UserID, conn sfu.ConnID, role sfu.Role,
) (*sfu.Conn, *sfutest.DirectSignaler) {
	t.Helper()
	c, sig, err := h.Join(sfu.JoinParams{
		Room: "lounge", Participant: sfu.ParticipantID(user), User: user, Conn: conn, Role: role,
		Decode: sfu.DecodeCaps{H264: []sfu.ProfileKey{sfu.ProfileHigh, sfu.ProfileConstrainedBaseline}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, sig
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func startShare(t *testing.T, c *sfu.Conn, id sfu.ShareID, preset sfu.Preset) {
	t.Helper()
	_, err := c.StartShare(testCtx(t), sfu.StartShareParams{ID: id, Preset: preset, Audio: true, Source: sfu.SourceScreen})
	if err != nil {
		t.Fatalf("StartShare(%s): %v", id, err)
	}
}

// newSource returns small fake media: two video layers and audio, light enough for any CI machine.
func newSource(t *testing.T) *fake.Source {
	t.Helper()
	src, err := fake.New(fake.Config{
		Layers: []fake.VideoLayer{
			{RID: "f", Width: 640, Height: 360, FPS: 30, Bitrate: 400_000},
			{RID: "q", Width: 320, Height: 180, FPS: 15, Bitrate: 100_000},
		},
		GOP: time.Second, Audio: true, Seed: 29,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	return src
}

// newPublisher returns a Publisher of src with audio and the given video layers: none means simulcast f and q, like
// the web sharer; a single layer goes out as a plain track without a rid.
func newPublisher(t *testing.T, src *fake.Source, se webrtc.SettingEngine, layers ...string) *publish.Publisher {
	t.Helper()
	pub, err := publish.New(publish.Options{Source: src, Settings: se, Audio: true, Layers: layers})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pub.Close(); err != nil {
			t.Errorf("Publisher.Close: %v", err)
		}
	})
	return pub
}

func newViewer(t *testing.T, se webrtc.SettingEngine) *sfutest.Viewer {
	t.Helper()
	v, err := sfutest.NewViewer(sfutest.ViewerOptions{Settings: se})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := v.Close(); err != nil {
			t.Errorf("Viewer.Close: %v", err)
		}
	})
	return v
}

// wantCode fails unless err is an *sfu.Error with the code and the Retryable flag of 02 §6.3.
func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var e *sfu.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("error = %v, want %s", err, code)
	}
	retryable := code == sfu.CodeBusy || code == sfu.CodePCRateLimited || code == sfu.CodeProbeLimit || code == sfu.CodeInternal
	if e.Retryable != retryable {
		t.Errorf("%s: Retryable = %v", code, e.Retryable)
	}
}

// eventually polls cond until it holds; it fails the test with what() after 15 s.
func eventually(t *testing.T, cond func() bool, what func() string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitShare waits until the share's ShareInfo satisfies cond and returns it.
func waitShare(t *testing.T, s *sfu.SFU, id sfu.ShareID, cond func(sfu.ShareInfo) bool) sfu.ShareInfo {
	t.Helper()
	var info sfu.ShareInfo
	eventually(t, func() bool {
		var ok bool
		info, ok = s.Share(id)
		return ok && cond(info)
	}, func() string { return fmt.Sprintf("share %s is %+v", id, info) })
	return info
}

func layerRIDs(info sfu.ShareInfo) []string {
	var out []string
	for _, l := range info.Layers {
		out = append(out, l.RID)
	}
	return out
}

func parse(t *testing.T, raw string) *sdp.SessionDescription {
	t.Helper()
	desc := &sdp.SessionDescription{}
	if err := desc.UnmarshalString(raw); err != nil {
		t.Fatalf("unparsable SDP: %v", err)
	}
	return desc
}

// section returns the m-section of a mid.
func section(t *testing.T, desc *sdp.SessionDescription, mid string) *sdp.MediaDescription {
	t.Helper()
	for _, md := range desc.MediaDescriptions {
		if got, _ := md.Attribute(sdp.AttrKeyMID); got == mid {
			return md
		}
	}
	t.Fatalf("no m-section with mid %q", mid)
	return nil
}

// directionOf returns the direction attribute of an m-section.
func directionOf(md *sdp.MediaDescription) string {
	for _, a := range md.Attributes {
		switch a.Key {
		case sdp.AttrKeySendRecv, sdp.AttrKeySendOnly, sdp.AttrKeyRecvOnly, sdp.AttrKeyInactive:
			return a.Key
		}
	}
	return ""
}

// attr returns the first attribute of an m-section with the key whose value starts with prefix.
func attr(md *sdp.MediaDescription, key, prefix string) string {
	for _, a := range md.Attributes {
		if a.Key == key && strings.HasPrefix(a.Value, prefix) {
			return a.Value
		}
	}
	return ""
}

// cand is one candidate of a server description.
type cand struct {
	proto   string // "udp" | "tcp"
	addr    netip.AddrPort
	typ     string
	passive bool
}

// candidates returns the component-1 candidates of an SDP, without repeats.
func candidates(t *testing.T, raw string) []cand {
	t.Helper()
	var out []cand
	for line := range strings.Lines(raw) {
		v, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "a=candidate:")
		if !ok {
			continue
		}
		c, err := ice.UnmarshalCandidate(v)
		if err != nil {
			t.Fatalf("unparsable candidate in the SDP: %v", err)
		}
		ip, err := netip.ParseAddr(c.Address())
		if err != nil {
			t.Fatalf("candidate address %q: %v", c.Address(), err)
		}
		if c.Component() != 1 {
			continue
		}
		got := cand{
			proto: c.NetworkType().NetworkShort(), addr: netip.AddrPortFrom(ip.Unmap(), uint16(c.Port())),
			typ: c.Type().String(), passive: c.TCPType() == ice.TCPTypePassive,
		}
		if !slices.Contains(out, got) {
			out = append(out, got)
		}
	}
	return out
}

// checkServerCandidates checks a description the SFU sent: complete (no trickle), and exactly the host candidates
// the Transport advertises, the TCP ones passive. It returns the candidates' transports ("udp", "tcp443", "tcp7882").
func checkServerCandidates(t *testing.T, h *sfutest.Harness, what, raw string) []string {
	t.Helper()
	if !strings.Contains(raw, "a=end-of-candidates") {
		t.Errorf("%s has no a=end-of-candidates: the SFU never trickles", what)
	}
	var vias []string
	got := candidates(t, raw)
	for _, c := range got {
		i := slices.IndexFunc(h.Transport.Advertised, func(a netx.AdvertisedAddr) bool {
			return a.Proto == c.proto && a.Addr == c.addr
		})
		switch {
		case i < 0:
			t.Errorf("%s: candidate %+v is not an advertised address", what, c)
			continue
		case c.typ != "host":
			t.Errorf("%s: candidate %+v is not a host candidate", what, c)
		case c.proto == "tcp" && !c.passive:
			t.Errorf("%s: TCP candidate %+v is not passive: the server never dials out", what, c)
		}
		vias = append(vias, h.Transport.Advertised[i].Via)
	}
	if len(got) != len(h.Transport.Advertised) {
		t.Errorf("%s has %d candidates, the Transport advertises %d", what, len(got), len(h.Transport.Advertised))
	}
	slices.Sort(vias)
	return vias
}

// TestPublishAndSubscribe is the slice's main acceptance test: a publisher and a viewer negotiate their pub and sub
// PCs through the SFU with gen, neg and tracks, on a Transport with UDP, TCP 7882 and the 443 multiplexer.
func TestPublishAndSubscribe(t *testing.T) {
	logs := &logBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h := newHarness(t, sfutest.HarnessOptions{TCP: true, TCP443: true, Logger: logger})
	ctx := testCtx(t)
	pubConn, pubSig := join(t, h, "alice", "c-pub", sfu.RoleFull)
	viewConn, viewSig := join(t, h, "bob", "c-view", sfu.RoleViewer)
	const share = sfu.ShareID("s_q7m2x9c4v8b1n5k3")
	startShare(t, pubConn, share, sfu.PresetMovie)
	allVias := []string{netx.ViaTCP443, netx.ViaTCP7882, netx.ViaUDP}

	// ---- publish: the client offers gen 1, neg 1 with its tracks bound to the share ----
	pub := newPublisher(t, newSource(t), sfutest.LoopbackSettings())
	answer, err := sfutest.Publish(ctx, pubConn, pub, 1, 1, share)
	if err != nil {
		t.Fatalf("publish negotiation: %v", err)
	}
	if vias := checkServerCandidates(t, h, "the pub answer", answer); !slices.Equal(vias, allVias) {
		t.Errorf("the pub answer's candidates are on %v, want %v", vias, allVias)
	}
	bindings := sfutest.Bindings(pub, share)
	if len(bindings) != 2 {
		t.Fatalf("the publisher's bindings = %+v, want a video and an audio m-section", bindings)
	}
	ans := parse(t, answer)
	for _, b := range bindings {
		md := section(t, ans, b.MID)
		if dir := directionOf(md); dir != sdp.AttrKeyRecvOnly {
			t.Errorf("answer m-section %s (%s) is %s, want recvonly", b.MID, b.Kind, dir)
		}
		switch b.Kind {
		case webrtc.RTPCodecTypeVideo:
			if sim := attr(md, "simulcast", ""); sim != "recv f;q" {
				t.Errorf("answer a=simulcast:%s, want recv f;q", sim)
			}
		case webrtc.RTPCodecTypeAudio:
			// The Movie preset: stereo Opus at 256 kbit/s (02 §8.4 step 5, §8.6).
			fmtp := attr(md, "fmtp", "111 ")
			for _, want := range []string{"stereo=1", "sprop-stereo=1", "maxaveragebitrate=256000"} {
				if !strings.Contains(fmtp, want) {
					t.Errorf("answer Opus fmtp %q lacks %s", fmtp, want)
				}
			}
		}
	}
	if err := pub.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pubSig.WaitPCState(ctx, sfu.PCPub, 1, "connected"); err != nil {
		t.Fatalf("pub PC: %v (events %+v)", err, pubSig.Events())
	}
	// The tracks attach to the share their m-sections are bound to: both simulcast layers and the audio.
	info := waitShare(t, h.SFU, share, func(i sfu.ShareInfo) bool { return len(i.Layers) == 2 && i.Audio })
	if got := layerRIDs(info); !slices.Equal(got, []string{"f", "q"}) {
		t.Errorf("share layers = %v, want f then q", got)
	}
	if info.State != sfu.SharePending || info.Conn != "c-pub" || info.Preset != sfu.PresetMovie {
		t.Errorf("ShareInfo = %+v", info)
	}
	tracks, err := pubConn.PubTracks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 3 {
		t.Fatalf("pub tracks = %+v, want f, q and audio", tracks)
	}
	for _, tr := range tracks {
		if tr.Share != share {
			t.Errorf("pub track %+v feeds %q, want the share its m-section is bound to", tr, tr.Share)
		}
	}

	// ---- subscribe: the SFU offers gen 1, neg 1 with a tracks binding ----
	viewer := newViewer(t, sfutest.LoopbackSettings())
	viewSig.Attach(viewConn, viewer)
	errs, err := viewConn.UpdateSubscriptions(ctx, []sfu.SubscriptionUpdate{{Share: share, Video: sfu.QualityHigh, Audio: true}})
	if err != nil || errs[0] != nil {
		t.Fatalf("UpdateSubscriptions = %v, %v", errs, err)
	}
	offer, err := viewSig.WaitOffer(ctx, func(sfutest.Offer) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if offer.PC != sfu.PCSub || offer.Gen != 1 || offer.Neg != 1 {
		t.Errorf("sub offer: pc %v, gen %d, neg %d; want sub, 1, 1", offer.PC, offer.Gen, offer.Neg)
	}
	if vias := checkServerCandidates(t, h, "the sub offer", offer.SDP); !slices.Equal(vias, allVias) {
		t.Errorf("the sub offer's candidates are on %v, want %v", vias, allVias)
	}
	// The passive TCP 443 candidate is the 443 multiplexer's port (netx.ListenPortMux on 127.0.0.1:0).
	want443 := cand{proto: "tcp", addr: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), h.TCP443Port()), typ: "host", passive: true}
	if h.TCP443Port() == 0 || !slices.Contains(candidates(t, offer.SDP), want443) {
		t.Errorf("the sub offer has no passive TCP candidate on the 443 multiplexer's port %d:\n%v", h.TCP443Port(),
			candidates(t, offer.SDP))
	}
	off := parse(t, offer.SDP)
	if len(offer.Tracks) != 2 || len(off.MediaDescriptions) != 2 {
		t.Fatalf("sub offer: %d bindings for %d m-sections, want 2 and 2", len(offer.Tracks), len(off.MediaDescriptions))
	}
	for i, b := range offer.Tracks {
		md := section(t, off, b.MID) // the binding names a real m-section
		wantKind, prefix := webrtc.RTPCodecTypeVideo, "v-"
		if i == 1 {
			wantKind, prefix = webrtc.RTPCodecTypeAudio, "a-"
		}
		if b.Share != share || b.Kind != wantKind || md.MediaName.Media != wantKind.String() {
			t.Errorf("sub binding %d = %+v on an m=%s section, want the share's %s", i, b, md.MediaName.Media, wantKind)
		}
		if msid := attr(md, "msid", ""); msid != string(share)+" "+prefix+string(share) {
			t.Errorf("sub m-section %s: msid %q", b.MID, msid)
		}
		if dir := directionOf(md); dir != sdp.AttrKeySendOnly {
			t.Errorf("sub m-section %s is %s, want sendonly", b.MID, dir)
		}
	}
	if fmtp := attr(section(t, off, offer.Tracks[1].MID), "fmtp", "111 "); !strings.Contains(fmtp, "stereo=1") {
		t.Errorf("sub offer Opus fmtp %q lacks stereo=1", fmtp)
	}
	if err := viewSig.WaitPCState(ctx, sfu.PCSub, 1, "connected"); err != nil {
		t.Fatalf("sub PC: %v (events %+v, answer errors %v)", err, viewSig.Events(), viewSig.Errs())
	}
	eventually(t, func() bool { return viewer.PC().ConnectionState() == webrtc.PeerConnectionStateConnected },
		func() string { return "the viewer's PC is " + viewer.PC().ConnectionState().String() })
	if errs := viewSig.Errs(); len(errs) != 0 {
		t.Errorf("answering the sub offer: %v", errs)
	}
	// The answer bound both DownTracks: the viewer takes every profile a publisher may send.
	var subs []sfu.SubscriptionState
	eventually(t, func() bool {
		subs, err = viewConn.Subscriptions(ctx)
		return err == nil && len(subs) == 1 && subs[0].VideoBound && subs[0].AudioBound
	}, func() string { return fmt.Sprintf("subscriptions = %+v, %v", subs, err) })
	if subs[0].Share != share || subs[0].Video != sfu.QualityHigh || !subs[0].Audio || len(subs[0].VideoPTs) != 5 {
		t.Errorf("subscription = %+v, want high, audio and a payload type for each of the five profiles", subs[0])
	}
	if v, a, ok := h.SFU.FanOut(share); !ok || v != 1 || a != 1 {
		t.Errorf("FanOut = %d video, %d audio, %v", v, a, ok)
	}
	if n := len(viewSig.Offers()); n != 1 {
		t.Errorf("%d sub offers, want 1", n)
	}
	// Neither side heard anything but its own PC's states.
	for name, sig := range map[string]*sfutest.DirectSignaler{"publisher": pubSig, "viewer": viewSig} {
		for _, ev := range sig.Events() {
			st, ok := ev.(sfu.PCStateEvent)
			if !ok || st.Gen != 1 || st.Reason != "" || (name == "publisher") != (st.PC == sfu.PCPub) {
				t.Errorf("the %s got %+v", name, ev)
			}
		}
	}
	if evs := h.Events.Events(); len(evs) != 0 {
		t.Errorf("room events without a state change: %+v", evs)
	}

	// ---- the publisher leaves: the share ends, and the viewer's m-sections go inactive ----
	pubConn.Close(sfu.EndReasonLeft)
	ended, err := h.Events.Wait(ctx, func(ev sfutest.RoomEvent) bool { return ev.Kind == sfutest.ShareEnded })
	if err != nil {
		t.Fatal(err)
	}
	if ended.Room != "lounge" || ended.Share.ID != share || ended.Reason != sfu.EndReasonLeft || len(ended.Share.Layers) != 2 {
		t.Errorf("ShareEnded = %+v", ended)
	}
	second, err := viewSig.WaitOffer(ctx, func(o sfutest.Offer) bool { return o.Neg == 2 })
	if err != nil {
		t.Fatal(err)
	}
	if second.Gen != 1 || len(second.Tracks) != 0 {
		t.Errorf("the offer after the share ended: gen %d, tracks %+v; want gen 1 and no bindings", second.Gen, second.Tracks)
	}
	for _, md := range parse(t, second.SDP).MediaDescriptions {
		if dir := directionOf(md); dir != sdp.AttrKeyInactive {
			t.Errorf("m-section of the ended share is %s, want inactive", dir)
		}
	}
	eventually(t, func() bool {
		subs, err = viewConn.Subscriptions(ctx)
		return err == nil && len(subs) == 0
	}, func() string { return fmt.Sprintf("subscriptions = %+v, %v", subs, err) })
	select {
	case <-pubConn.Done():
	case <-ctx.Done():
		t.Error("the publisher's Conn did not finish closing")
	}

	// At info and above the SFU's own lines hold ids and states, never SDP, candidates or addresses (02 §13). Pion's
	// lines come through logx's bridge with a scope attribute; what they may say is logx's (04 §10).
	var sfuLines int
	for line := range strings.Lines(logs.String()) {
		if !strings.Contains(line, "component=sfu") || strings.Contains(line, " scope=") {
			continue
		}
		sfuLines++
		for _, secret := range []string{"v=0", "candidate", "ice-ufrag", "ice-pwd", "fingerprint", "127.0.0.1", "a=", "m="} {
			if strings.Contains(line, secret) {
				t.Errorf("an SFU log line at info holds %q: %s", secret, line)
			}
		}
	}
	for _, want := range []string{
		`msg="connection joined"`, `msg="share started"`, "conn_id=c-pub room_id=lounge user_id=alice pc=pub gen=1 state=connected",
		"conn_id=c-view room_id=lounge user_id=bob pc=sub gen=1 state=connected", `msg="share ended"`, `msg="connection left"`,
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the logs lack %s (%d SFU lines)", want, sfuLines)
		}
	}
}

// logBuffer is a log sink that several goroutines write to.
type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (w *logBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *logBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// TestPubOfferGenAndNeg: the pub PC's gen and neg rules (02 §5.3, 01 §9): a repeated neg gets the stored answer, a
// lower neg or gen is sfu.stale_offer and changes nothing, and a higher gen replaces the PC while the share lives on.
func TestPubOfferGenAndNeg(t *testing.T) {
	h := newHarness(t, sfutest.HarnessOptions{})
	ctx := testCtx(t)
	conn, sig := join(t, h, "alice", "c-pub", sfu.RoleFull)
	const share = sfu.ShareID("s_gen")
	startShare(t, conn, share, sfu.PresetAuto)
	src := newSource(t)

	// The first PC publishes one video layer without a rid, so that it can re-offer: Pion's re-offer of a simulcast
	// m-section lists every rid twice (recv and send), which the SFU refuses as sfu.bad_rid like any repeated rid.
	// publish.Publisher has to leave the recv lines out of its re-offers before it can renegotiate simulcast.
	pub := newPublisher(t, src, sfutest.LoopbackSettings(), "f")
	offer, err := pub.Offer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bindings := sfutest.Bindings(pub, share)
	first, err := conn.HandleOffer(ctx, sfu.PCPub, 1, 1, offer.SDP, bindings)
	if err != nil {
		t.Fatal(err)
	}
	// A repeated neg (a replay after a resume) gets the stored answer, whatever it carries.
	for _, sdp := range []string{offer.SDP, "replayed"} {
		again, err := conn.HandleOffer(ctx, sfu.PCPub, 1, 1, sdp, bindings)
		if err != nil || again != first {
			t.Errorf("repeated neg: err %v, the same answer: %v", err, again == first)
		}
	}
	if err := pub.SetAnswer(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: first}); err != nil {
		t.Fatal(err)
	}
	if err := pub.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := sig.WaitPCState(ctx, sfu.PCPub, 1, "connected"); err != nil {
		t.Fatal(err)
	}
	one := waitShare(t, h.SFU, share, func(i sfu.ShareInfo) bool { return len(i.Layers) == 1 && i.Audio })
	if got := layerRIDs(one); !slices.Equal(got, []string{"f"}) {
		t.Errorf("layers of a video m-section without rids = %v, want a single f", got)
	}

	// A re-offer in the same gen (neg 2) is answered on the same PC, with a new answer.
	second, err := sfutest.Publish(ctx, conn, pub, 1, 2, share)
	if err != nil {
		t.Fatalf("re-offer: %v", err)
	}
	if again, err := conn.HandleOffer(ctx, sfu.PCPub, 1, 2, "replayed", nil); err != nil || again != second {
		t.Errorf("repeated neg 2: err %v, the same answer: %v", err, again == second)
	}
	// Stale offers: a lower neg in this gen, and gens and negs that count from 1.
	stale := func(gen, neg uint32) {
		t.Helper()
		before, _ := h.SFU.Share(share)
		tracksBefore, _ := conn.PubTracks(ctx)
		_, err := conn.HandleOffer(ctx, sfu.PCPub, gen, neg, offer.SDP, bindings)
		wantCode(t, err, sfu.CodeStaleOffer)
		after, _ := h.SFU.Share(share)
		tracksAfter, _ := conn.PubTracks(ctx)
		if len(after.Layers) != len(before.Layers) || after.Audio != before.Audio || len(tracksAfter) != len(tracksBefore) {
			t.Errorf("a stale offer (gen %d, neg %d) changed the share: %+v → %+v", gen, neg, before, after)
		}
	}
	stale(1, 1)
	stale(0, 1)
	stale(1, 0)

	// The client rebuilds its pub PC: gen 2, neg 1, the same share id in tracks. The old PC goes, the Share stays, and
	// the new PC's tracks (simulcast this time) attach to it.
	pub2 := newPublisher(t, src, sfutest.LoopbackSettings())
	if _, err := sfutest.Publish(ctx, conn, pub2, 2, 1, share); err != nil {
		t.Fatalf("gen 2: %v", err)
	}
	if info, _ := h.SFU.Share(share); len(info.Layers) != 0 || info.Audio {
		t.Errorf("the share right after its pub PC was replaced = %+v, want the old PC's layers gone", info)
	}
	if err := pub.Close(); err != nil { // the Source has one consumer at a time
		t.Fatal(err)
	}
	if err := pub2.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := sig.WaitPCState(ctx, sfu.PCPub, 2, "connected"); err != nil {
		t.Fatal(err)
	}
	info := waitShare(t, h.SFU, share, func(i sfu.ShareInfo) bool { return len(i.Layers) == 2 && i.Audio })
	if info.ID != share || info.State != sfu.SharePending {
		t.Errorf("the share after the rebuild = %+v", info)
	}
	stale(1, 3) // the old gen is over, whatever its neg
	stale(1, 9)
	stale(2, 0)
	// PC states are reported for the current PC only: the replaced gen 1 PC closes silently.
	for _, ev := range sig.Events() {
		if st, ok := ev.(sfu.PCStateEvent); ok && st.Gen == 1 && st.State == "closed" {
			t.Errorf("the replaced PC reported %+v", st)
		}
	}
	if evs := h.Events.Events(); len(evs) != 0 {
		t.Errorf("room events: %+v, want none: a pub PC rebuild doesn't end the share", evs)
	}

	// The client closes its current pub PC (Pion closes the server side on the DTLS close_notify): the SFU reports
	// it, the share loses its layers but lives on (the hub owns its lifecycle), and this gen takes no more offers.
	if err := pub2.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sig.WaitPCState(ctx, sfu.PCPub, 2, "closed"); err != nil {
		t.Fatalf("%v (events %+v)", err, sig.Events())
	}
	waitShare(t, h.SFU, share, func(i sfu.ShareInfo) bool { return len(i.Layers) == 0 && !i.Audio })
	_, err = conn.HandleOffer(ctx, sfu.PCPub, 2, 2, offer.SDP, bindings)
	wantCode(t, err, sfu.CodeBadPC)
	stale(1, 1)
}

// rawPublisher is a Pion client with several sendonly tracks on one PeerConnection, for what a publish.Publisher
// doesn't do: more than one share on a pub PC.
type rawPublisher struct {
	pc     *webrtc.PeerConnection
	tracks []*webrtc.TrackLocalStaticRTP
	mids   []string // per track, known after offer
}

func newRawPublisher(t *testing.T, kinds ...webrtc.RTPCodecType) *rawPublisher {
	t.Helper()
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(sfutest.LoopbackSettings())).
		NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pc.Close(); err != nil {
			t.Errorf("raw publisher close: %v", err)
		}
	})
	p := &rawPublisher{pc: pc}
	for i, kind := range kinds {
		track := rawTrack(t, kind, fmt.Sprintf("t%d", i))
		init := webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly}
		if _, err := pc.AddTransceiverFromTrack(track, init); err != nil {
			t.Fatal(err)
		}
		p.tracks = append(p.tracks, track)
	}
	return p
}

// rawTrack returns a local track of a kind for a raw publisher: H.264 constrained baseline, or stereo Opus.
func rawTrack(t *testing.T, kind webrtc.RTPCodecType, id string) *webrtc.TrackLocalStaticRTP {
	t.Helper()
	codec := webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
		SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
	}
	if kind == webrtc.RTPCodecTypeAudio {
		codec = webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}
	}
	track, err := webrtc.NewTrackLocalStaticRTP(codec, id, "raw")
	if err != nil {
		t.Fatal(err)
	}
	return track
}

// sendUntil writes one RTP packet on track every 20 ms until cond holds; it fails the test with what after 15 s.
func sendUntil(t *testing.T, track *webrtc.TrackLocalStaticRTP, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for seq := uint16(1); !cond(); seq++ {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		pkt := &rtp.Packet{
			Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: uint32(seq) * 3000},
			Payload: []byte{0x41, 0x9a, 0x00, 0x01},
		}
		_ = track.WriteRTP(pkt) // errors before the PC is connected are expected
		time.Sleep(20 * time.Millisecond)
	}
}

// withoutLines returns an SDP without the lines that start with prefix.
func withoutLines(raw, prefix string) string {
	var b strings.Builder
	for line := range strings.Lines(raw) {
		if !strings.HasPrefix(line, prefix) {
			b.WriteString(line)
		}
	}
	return b.String()
}

// offer creates the next offer with every candidate in it and records the tracks' mids.
func (p *rawPublisher) offer(t *testing.T, ctx context.Context) string {
	t.Helper()
	offer, err := p.pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(p.pc)
	if err := p.pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gathered:
	case <-ctx.Done():
		t.Fatal("the raw publisher's gathering did not finish")
	}
	p.mids = p.mids[:0]
	for _, tr := range p.pc.GetTransceivers() {
		p.mids = append(p.mids, tr.Mid())
	}
	return p.pc.LocalDescription().SDP
}

// apply sets the SFU's answer on the client.
func (p *rawPublisher) apply(t *testing.T, answer string) {
	t.Helper()
	if err := p.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		t.Fatalf("the raw publisher refused the answer: %v", err)
	}
}

// send writes one RTP packet on every track, every 20 ms, until the test ends.
func (p *rawPublisher) send(t *testing.T) {
	t.Helper()
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		var seq uint16
		for {
			select {
			case <-done:
				return
			case <-tick.C:
			}
			seq++
			for _, track := range p.tracks {
				pkt := &rtp.Packet{
					Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: uint32(seq) * 960},
					Payload: []byte{0x41, 0x9a, 0x00, 0x01},
				}
				_ = track.WriteRTP(pkt) // errors before the PC is connected are expected
			}
		}
	}()
	t.Cleanup(func() {
		close(done)
		<-stopped
	})
}

// TestPubOfferForStoppedShare: a share stopped while its pub offer was in flight (02 §6.3, §8.4). The offer still
// binds it; the SFU answers anyway, with that share's m-sections a=inactive, attaches nothing that arrives on them,
// and the publisher's other share is untouched. The next offer may bind the same m-sections to a new share.
func TestPubOfferForStoppedShare(t *testing.T) {
	h := newHarness(t, sfutest.HarnessOptions{})
	ctx := testCtx(t)
	conn, sig := join(t, h, "alice", "c-pub", sfu.RoleFull)
	const gone, kept, next = sfu.ShareID("s_gone"), sfu.ShareID("s_kept"), sfu.ShareID("s_next")
	startShare(t, conn, gone, sfu.PresetAuto)
	startShare(t, conn, kept, sfu.PresetAuto)

	video, audio := webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio
	pub := newRawPublisher(t, video, video, audio) // gone's video, kept's video, gone's audio
	offer := pub.offer(t, ctx)
	bind := func(first sfu.ShareID) []sfu.TrackBinding {
		return []sfu.TrackBinding{
			{MID: pub.mids[0], Share: first, Kind: video},
			{MID: pub.mids[1], Share: kept, Kind: video},
			{MID: pub.mids[2], Share: first, Kind: audio},
		}
	}

	// share.stop crosses the offer: the hub ends the share first, then hands the offer over with its binding.
	if err := conn.StopShare(ctx, gone, sfu.EndReasonStopped); err != nil {
		t.Fatal(err)
	}
	answer, err := conn.HandleOffer(ctx, sfu.PCPub, 1, 1, offer, bind(gone))
	if err != nil {
		t.Fatalf("the offer that binds a stopped share: %v, want an answer", err)
	}
	ans := parse(t, answer)
	for i, want := range []string{sdp.AttrKeyInactive, sdp.AttrKeyRecvOnly, sdp.AttrKeyInactive} {
		if dir := directionOf(section(t, ans, pub.mids[i])); dir != want {
			t.Errorf("answer m-section %s is %s, want %s", pub.mids[i], dir, want)
		}
	}
	if _, ok := h.SFU.Share(gone); ok {
		t.Error("the stopped share is listed again")
	}

	// A client that sends on the inactive m-sections anyway (Pion does): its packets are read and discarded, never
	// attached; the other share gets its layer.
	pub.apply(t, answer)
	pub.send(t)
	if err := sig.WaitPCState(ctx, sfu.PCPub, 1, "connected"); err != nil {
		t.Fatal(err)
	}
	wantTracks := func(shares ...sfu.ShareID) {
		t.Helper()
		var tracks []sfu.PubTrackState
		eventually(t, func() bool {
			var err error
			tracks, err = conn.PubTracks(ctx)
			if err != nil || len(tracks) != 3 {
				return false
			}
			for i, tr := range tracks {
				if tr.MID != pub.mids[i] || tr.Share != shares[i] || tr.Packets == 0 {
					return false
				}
			}
			return true
		}, func() string {
			return fmt.Sprintf("pub tracks = %+v, want them on mids %v feeding %q", tracks, pub.mids, shares)
		})
	}
	wantTracks("", kept, "")
	if info := waitShare(t, h.SFU, kept, func(i sfu.ShareInfo) bool { return len(i.Layers) == 1 }); info.Audio ||
		info.Layers[0].RID != "f" {
		t.Errorf("the kept share = %+v, want one f layer and no audio", info)
	}
	// The drained tracks keep being read: their counters grow.
	before, _ := conn.PubTracks(ctx)
	eventually(t, func() bool {
		now, err := conn.PubTracks(ctx)
		return err == nil && len(now) == 3 && now[0].Packets > before[0].Packets+5 && now[2].Packets > before[2].Packets+5
	}, func() string { return "the tracks without a share are no longer read" })

	// The client's next share reuses the m-sections: the next offer binds them, and the same tracks now feed it.
	startShare(t, conn, next, sfu.PresetMovie)
	answer, err = conn.HandleOffer(ctx, sfu.PCPub, 1, 2, pub.offer(t, ctx), bind(next))
	if err != nil {
		t.Fatal(err)
	}
	ans = parse(t, answer)
	for _, mid := range pub.mids {
		if dir := directionOf(section(t, ans, mid)); dir != sdp.AttrKeyRecvOnly {
			t.Errorf("after rebinding, answer m-section %s is %s, want recvonly", mid, dir)
		}
	}
	if fmtp := attr(section(t, ans, pub.mids[2]), "fmtp", ""); !strings.Contains(fmtp, "maxaveragebitrate=256000") {
		t.Errorf("the rebound audio m-section's fmtp %q lacks the Movie preset's bitrate", fmtp)
	}
	pub.apply(t, answer)
	wantTracks(next, kept, next)
	waitShare(t, h.SFU, next, func(i sfu.ShareInfo) bool { return len(i.Layers) == 1 && i.Audio })

	// An offer whose tracks no longer bind a live share's m-sections detaches them (the hub stops such a share).
	answer, err = conn.HandleOffer(ctx, sfu.PCPub, 1, 3, pub.offer(t, ctx), bind(next)[1:2])
	if err != nil {
		t.Fatal(err)
	}
	pub.apply(t, answer)
	wantTracks("", kept, "")
	waitShare(t, h.SFU, next, func(i sfu.ShareInfo) bool { return len(i.Layers) == 0 && !i.Audio })

	want := []string{"s_gone:stopped"}
	var got []string
	for _, ev := range h.Events.Events() {
		got = append(got, fmt.Sprintf("%s:%s", ev.Share.ID, ev.Reason))
	}
	if !slices.Equal(got, want) {
		t.Errorf("room events = %v, want %v", got, want)
	}
}

// TestPubSectionReuse: a client that stops a share takes its track off the m-section, which goes inactive, and gives
// the same m-section to its next share, as browsers do (02 §8.4 step 5). The inactive offer ends the first share's
// track (Pion stops its receiver, 02 §5.1): it leaves the pub PC, so the offer that binds the m-section to the next
// share finds nothing to attach. That share gets its layer from the track Pion delivers once its media flows.
func TestPubSectionReuse(t *testing.T) {
	h := newHarness(t, sfutest.HarnessOptions{})
	ctx := testCtx(t)
	conn, sig := join(t, h, "alice", "c-pub", sfu.RoleFull)
	const first, second = sfu.ShareID("s_first"), sfu.ShareID("s_second")
	startShare(t, conn, first, sfu.PresetAuto)

	video := webrtc.RTPCodecTypeVideo
	pub := newRawPublisher(t, video)
	offer := pub.offer(t, ctx)
	mid := pub.mids[0]
	bind := func(share sfu.ShareID) []sfu.TrackBinding {
		return []sfu.TrackBinding{{MID: mid, Share: share, Kind: video}}
	}
	// negotiate hands an offer to the SFU and its answer to the client, and returns the answer's direction for mid.
	negotiate := func(neg uint32, offer string, tracks []sfu.TrackBinding) string {
		t.Helper()
		answer, err := conn.HandleOffer(ctx, sfu.PCPub, 1, neg, offer, tracks)
		if err != nil {
			t.Fatalf("pub offer neg %d: %v", neg, err)
		}
		pub.apply(t, answer)
		return directionOf(section(t, parse(t, answer), mid))
	}
	pubTracks := func() []sfu.PubTrackState {
		t.Helper()
		tracks, err := conn.PubTracks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return tracks
	}
	layers := func(id sfu.ShareID) []string {
		info, _ := h.SFU.Share(id)
		return layerRIDs(info)
	}

	negotiate(1, offer, bind(first))
	if err := sig.WaitPCState(ctx, sfu.PCPub, 1, "connected"); err != nil {
		t.Fatal(err)
	}
	sendUntil(t, pub.tracks[0], func() bool { return len(layers(first)) == 1 }, "the first share has no layer")
	if tracks := pubTracks(); len(tracks) != 1 || tracks[0].MID != mid || tracks[0].Share != first {
		t.Fatalf("pub tracks = %+v, want one on mid %s feeding %s", tracks, mid, first)
	}

	// The share stops and the client removes its track: the m-section is inactive in the next offer.
	if err := conn.StopShare(ctx, first, sfu.EndReasonStopped); err != nil {
		t.Fatal(err)
	}
	if err := pub.pc.RemoveTrack(pub.pc.GetTransceivers()[0].Sender()); err != nil {
		t.Fatal(err)
	}
	offer = pub.offer(t, ctx)
	if dir := directionOf(section(t, parse(t, offer), mid)); dir != sdp.AttrKeyInactive {
		t.Fatalf("the client's offer after RemoveTrack: m-section %s is %s, want inactive", mid, dir)
	}
	if dir := negotiate(2, offer, nil); dir != sdp.AttrKeyInactive {
		t.Errorf("answer to the inactive m-section is %s, want inactive", dir)
	}
	var tracks []sfu.PubTrackState
	eventually(t, func() bool {
		tracks = pubTracks()
		return len(tracks) == 0
	}, func() string {
		return fmt.Sprintf("pub tracks after the m-section went inactive = %+v, want none: its track has ended", tracks)
	})

	// The next share takes the m-section over, with a new track.
	startShare(t, conn, second, sfu.PresetAuto)
	next := rawTrack(t, video, "t-next")
	if _, err := pub.pc.AddTrack(next); err != nil {
		t.Fatal(err)
	}
	offer = pub.offer(t, ctx)
	if dir := directionOf(section(t, parse(t, offer), mid)); len(pub.mids) != 1 || dir != sdp.AttrKeySendOnly {
		t.Fatalf("the client's offer after AddTrack: mids %v, m-section %s is %s; want the m-section reused, sendonly",
			pub.mids, mid, dir)
	}
	if dir := negotiate(3, offer, bind(second)); dir != sdp.AttrKeyRecvOnly {
		t.Errorf("answer to the reused m-section is %s, want recvonly", dir)
	}
	// Before the new track's first packet nothing feeds the share: the ended track is not bound to it.
	if got := layers(second); len(got) != 0 {
		t.Errorf("the next share before its media has layers %v, want none: the first share's track has ended", got)
	}
	if tracks := pubTracks(); len(tracks) != 0 {
		t.Errorf("pub tracks before the next share's media = %+v, want none", tracks)
	}
	sendUntil(t, next, func() bool {
		tracks = pubTracks()
		return slices.Equal(layers(second), []string{"f"}) && len(tracks) == 1 && tracks[0].Packets > 0
	}, "the next share has no f layer from its own track")
	if tracks[0].MID != mid || tracks[0].Share != second {
		t.Errorf("pub tracks = %+v, want one on mid %s feeding %s", tracks, mid, second)
	}
}

// TestPubOfferRefusedByPion: an offer that passes the SFU's checks but that Pion refuses after taking it as its
// remote offer leaves the pub PC unable to negotiate: Pion has no rollback. The PC and its media stay, every further
// offer of its gen is sfu.bad_sdp (01's sdp_invalid, on which the client rebuilds), and the gen + 1 offer replaces it.
// An offer the SFU's own checks refuse changes nothing.
func TestPubOfferRefusedByPion(t *testing.T) {
	h := newHarness(t, sfutest.HarnessOptions{})
	ctx := testCtx(t)
	conn, sig := join(t, h, "alice", "c-pub", sfu.RoleFull)
	const share = sfu.ShareID("s_1")
	startShare(t, conn, share, sfu.PresetAuto)

	video := webrtc.RTPCodecTypeVideo
	pub := newRawPublisher(t, video)
	offer := pub.offer(t, ctx)
	bindings := []sfu.TrackBinding{{MID: pub.mids[0], Share: share, Kind: video}}
	first, err := conn.HandleOffer(ctx, sfu.PCPub, 1, 1, offer, bindings)
	if err != nil {
		t.Fatal(err)
	}
	pub.apply(t, first)
	pub.send(t)
	if err := sig.WaitPCState(ctx, sfu.PCPub, 1, "connected"); err != nil {
		t.Fatal(err)
	}
	waitShare(t, h.SFU, share, func(i sfu.ShareInfo) bool { return len(i.Layers) == 1 })

	wantStuck := func(want bool, when string) {
		t.Helper()
		if stuck, err := conn.PubStuck(ctx); err != nil || stuck != want {
			t.Fatalf("%s: the pub PC is stuck: %v, %v; want %v", when, stuck, err, want)
		}
	}

	// Refused by the SFU's own checks, before Pion sees it: the same neg is fine afterwards.
	_, err = conn.HandleOffer(ctx, sfu.PCPub, 1, 2, "v=nonsense", bindings)
	wantCode(t, err, sfu.CodeBadSDP)
	wantStuck(false, "after an offer the SFU's checks refused")
	second, err := conn.HandleOffer(ctx, sfu.PCPub, 1, 2, pub.offer(t, ctx), bindings)
	if err != nil {
		t.Fatalf("the re-offer after one the SFU's checks refused: %v", err)
	}
	pub.apply(t, second)

	// Refused by Pion: the offer lost its ICE user name, which Pion looks for only after it has taken the offer.
	offer = pub.offer(t, ctx)
	_, err = conn.HandleOffer(ctx, sfu.PCPub, 1, 3, withoutLines(offer, "a=ice-ufrag:"), bindings)
	wantCode(t, err, sfu.CodeBadSDP)
	wantStuck(true, "after an offer Pion refused")
	// Pion still holds that offer, so the right one can't be applied either, nor can any later one of this gen.
	for _, neg := range []uint32{3, 4} {
		_, err = conn.HandleOffer(ctx, sfu.PCPub, 1, neg, offer, bindings)
		wantCode(t, err, sfu.CodeBadSDP)
	}
	// The answered negs are as before: the last one gets its stored answer, an older one is stale.
	if again, err := conn.HandleOffer(ctx, sfu.PCPub, 1, 2, "replayed", nil); err != nil || again != second {
		t.Errorf("repeated neg 2 on the stuck PC: err %v, the same answer: %v", err, again == second)
	}
	_, err = conn.HandleOffer(ctx, sfu.PCPub, 1, 1, offer, bindings)
	wantCode(t, err, sfu.CodeStaleOffer)
	// The PC and its media stay: the track still feeds the share and is still read.
	before, err := conn.PubTracks(ctx)
	if err != nil || len(before) != 1 || before[0].Share != share {
		t.Fatalf("pub tracks on the stuck PC = %+v, %v; want the share's track", before, err)
	}
	eventually(t, func() bool {
		now, err := conn.PubTracks(ctx)
		return err == nil && len(now) == 1 && now[0].Share == share && now[0].Packets > before[0].Packets+5
	}, func() string { return "the stuck PC's track is no longer read" })
	if info, ok := h.SFU.Share(share); !ok || len(info.Layers) != 1 {
		t.Errorf("the share on the stuck PC = %+v, %v; want its layer kept", info, ok)
	}

	// The client rebuilds: gen 2 replaces the stuck PC and negotiates as usual, re-offers included.
	pub2 := newRawPublisher(t, video)
	offer = pub2.offer(t, ctx)
	bindings = []sfu.TrackBinding{{MID: pub2.mids[0], Share: share, Kind: video}}
	answer, err := conn.HandleOffer(ctx, sfu.PCPub, 2, 1, offer, bindings)
	if err != nil {
		t.Fatalf("gen 2 after the stuck gen 1: %v", err)
	}
	wantStuck(false, "after the rebuild")
	pub2.apply(t, answer)
	pub2.send(t)
	if err := sig.WaitPCState(ctx, sfu.PCPub, 2, "connected"); err != nil {
		t.Fatal(err)
	}
	waitShare(t, h.SFU, share, func(i sfu.ShareInfo) bool { return len(i.Layers) == 1 })
	if answer, err = conn.HandleOffer(ctx, sfu.PCPub, 2, 2, pub2.offer(t, ctx), bindings); err != nil {
		t.Fatalf("the re-offer on gen 2: %v", err)
	}
	pub2.apply(t, answer)
	if evs := h.Events.Events(); len(evs) != 0 {
		t.Errorf("room events: %+v, want none: the share lives through all of it", evs)
	}
}

// TestSubNegotiation: the sub PC's negotiation states (02 §5.3): one offer outstanding at a time, changes made
// meanwhile folded into one follow-up offer, answers matched by gen and neg, and inactive m-sections when a share
// ends.
func TestSubNegotiation(t *testing.T) {
	h := newHarness(t, sfutest.HarnessOptions{})
	ctx := testCtx(t)
	pubConn, _ := join(t, h, "alice", "c-pub", sfu.RoleFull)
	viewConn, sig := join(t, h, "bob", "c-view", sfu.RoleViewer)
	startShare(t, pubConn, "s_1", sfu.PresetAuto)
	startShare(t, pubConn, "s_2", sfu.PresetAuto)
	viewer := newViewer(t, sfutest.LoopbackSettings()) // answered by hand: no Attach
	subscribe := func(id sfu.ShareID) {
		t.Helper()
		errs, err := viewConn.UpdateSubscriptions(ctx, []sfu.SubscriptionUpdate{{Share: id, Video: sfu.QualityLow}})
		if err != nil || errs[0] != nil {
			t.Fatalf("subscribe %s: %v, %v", id, errs, err)
		}
	}
	waitOffer := func(neg uint32) sfutest.Offer {
		t.Helper()
		o, err := sig.WaitOffer(ctx, func(o sfutest.Offer) bool { return o.Neg == neg })
		if err != nil {
			t.Fatalf("sub offer neg %d: %v (offers so far: %d)", neg, err, len(sig.Offers()))
		}
		if o.PC != sfu.PCSub || o.Gen != 1 {
			t.Errorf("sub offer neg %d: pc %v, gen %d", neg, o.PC, o.Gen)
		}
		return o
	}
	answerTo := func(o sfutest.Offer) string {
		t.Helper()
		a, err := viewer.Answer(ctx, webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: o.SDP})
		if err != nil {
			t.Fatal(err)
		}
		return a.SDP
	}
	shares := func(o sfutest.Offer) []sfu.ShareID {
		var out []sfu.ShareID
		for _, b := range o.Tracks {
			out = append(out, b.Share)
		}
		return out
	}

	subscribe("s_1")
	first := waitOffer(1)
	if got := shares(first); !slices.Equal(got, []sfu.ShareID{"s_1", "s_1"}) {
		t.Errorf("offer 1 binds %v", got)
	}
	// A change while the offer is outstanding waits for the answer.
	subscribe("s_2")
	time.Sleep(200 * time.Millisecond) // four debounce periods
	if n := len(sig.Offers()); n != 1 {
		t.Fatalf("%d sub offers while the first is outstanding, want 1", n)
	}
	answer1 := answerTo(first)
	// Answers that don't match the outstanding offer are stale and change nothing; so is an answer for the pub PC.
	wantCode(t, viewConn.HandleAnswer(ctx, sfu.PCSub, 1, 2, answer1), sfu.CodeStaleAnswer)
	wantCode(t, viewConn.HandleAnswer(ctx, sfu.PCSub, 2, 1, answer1), sfu.CodeStaleAnswer)
	wantCode(t, viewConn.HandleAnswer(ctx, sfu.PCSub, 0, 1, answer1), sfu.CodeStaleAnswer)
	wantCode(t, viewConn.HandleAnswer(ctx, sfu.PCPub, 1, 1, answer1), sfu.CodeBadPC)
	// An answer the SFU's own checks refuse leaves the offer outstanding (one that Pion refuses closes the PC:
	// TestSubAnswerRefusedByPion).
	wantCode(t, viewConn.HandleAnswer(ctx, sfu.PCSub, 1, 1, "v=nonsense"), sfu.CodeBadSDP)
	wantCode(t, viewConn.HandleAnswer(ctx, sfu.PCSub, 1, 1, strings.Replace(answer1, "m=video", "m=application", 1)),
		sfu.CodeBadSDP)
	if n := len(sig.Offers()); n != 1 {
		t.Fatalf("%d sub offers after refused answers, want 1", n)
	}
	if err := viewConn.HandleAnswer(ctx, sfu.PCSub, 1, 1, answer1); err != nil {
		t.Fatalf("answer 1: %v", err)
	}
	wantCode(t, viewConn.HandleAnswer(ctx, sfu.PCSub, 1, 1, answer1), sfu.CodeStaleAnswer) // already applied

	// The folded change follows at once as neg 2.
	second := waitOffer(2)
	if got := shares(second); !slices.Equal(got, []sfu.ShareID{"s_1", "s_1", "s_2", "s_2"}) {
		t.Errorf("offer 2 binds %v", got)
	}
	if err := viewConn.HandleAnswer(ctx, sfu.PCSub, 1, 2, answerTo(second)); err != nil {
		t.Fatalf("answer 2: %v", err)
	}
	if err := sig.WaitPCState(ctx, sfu.PCSub, 1, "connected"); err != nil {
		t.Fatal(err)
	}

	// A share ends: its subscription goes, and the next offer makes its m-sections inactive and keeps the others.
	if err := pubConn.StopShare(ctx, "s_1", sfu.EndReasonStopped); err != nil {
		t.Fatal(err)
	}
	third := waitOffer(3)
	if got := shares(third); !slices.Equal(got, []sfu.ShareID{"s_2", "s_2"}) {
		t.Errorf("offer 3 binds %v, want only the share that is left", got)
	}
	desc := parse(t, third.SDP)
	for _, b := range second.Tracks {
		want := sdp.AttrKeySendOnly
		if b.Share == "s_1" {
			want = sdp.AttrKeyInactive
		}
		if dir := directionOf(section(t, desc, b.MID)); dir != want {
			t.Errorf("offer 3: m-section %s (%s) is %s, want %s", b.MID, b.Share, dir, want)
		}
	}
	if err := viewConn.HandleAnswer(ctx, sfu.PCSub, 1, 3, answerTo(third)); err != nil {
		t.Fatalf("answer 3: %v", err)
	}
	// A subscription to a share that has ended is refused per item, and makes no offer.
	errs, err := viewConn.UpdateSubscriptions(ctx, []sfu.SubscriptionUpdate{{Share: "s_1", Video: sfu.QualityHigh}})
	if err != nil {
		t.Fatal(err)
	}
	wantCode(t, errs[0], sfu.CodeShareNotFound)

	// The viewer leaves: its DownTracks leave the share.
	if v, a, ok := h.SFU.FanOut("s_2"); !ok || v != 1 || a != 1 {
		t.Errorf("FanOut(s_2) = %d, %d, %v", v, a, ok)
	}
	viewConn.Close(sfu.EndReasonLeft)
	select {
	case <-viewConn.Done():
	case <-ctx.Done():
		t.Fatal("the viewer's Conn did not finish closing")
	}
	if v, a, ok := h.SFU.FanOut("s_2"); !ok || v != 0 || a != 0 {
		t.Errorf("FanOut(s_2) after the viewer left = %d, %d, %v", v, a, ok)
	}
	if n := len(sig.Offers()); n != 3 {
		t.Errorf("%d sub offers in all, want 3", n)
	}
}

// TestSubPCClosedByClient: a viewer closes its sub PC (Pion closes the server side on the DTLS close_notify). The
// SFU reports it and offers nothing more on that PC; the Conn and its subscriptions stay. Building a new sub PC from
// closed is README S57's, so until then a new subscription fails with a retryable sfu.internal.
func TestSubPCClosedByClient(t *testing.T) {
	h := newHarness(t, sfutest.HarnessOptions{})
	ctx := testCtx(t)
	pubConn, _ := join(t, h, "alice", "c-pub", sfu.RoleFull)
	viewConn, sig := join(t, h, "bob", "c-view", sfu.RoleViewer)
	startShare(t, pubConn, "s_1", sfu.PresetAuto)
	startShare(t, pubConn, "s_2", sfu.PresetAuto)
	viewer := newViewer(t, sfutest.LoopbackSettings())
	sig.Attach(viewConn, viewer)
	if _, err := viewConn.UpdateSubscriptions(ctx, []sfu.SubscriptionUpdate{{Share: "s_1", Video: sfu.QualityLow}}); err != nil {
		t.Fatal(err)
	}
	if err := sig.WaitPCState(ctx, sfu.PCSub, 1, "connected"); err != nil {
		t.Fatalf("%v (answer errors %v)", err, sig.Errs())
	}
	// The SFU is the DTLS server, whose handshake is done one flight before the client's: a viewer closed before its
	// own side is connected has no DTLS connection to send a close_notify on, and the SFU would only see ICE fail.
	eventually(t, func() bool { return viewer.PC().ConnectionState() == webrtc.PeerConnectionStateConnected },
		func() string { return "the viewer's PC is " + viewer.PC().ConnectionState().String() })

	if err := viewer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sig.WaitPCState(ctx, sfu.PCSub, 1, "closed"); err != nil {
		t.Fatalf("%v (events %+v)", err, sig.Events())
	}
	wantCode(t, viewConn.HandleAnswer(ctx, sfu.PCSub, 1, 1, "v=0"), sfu.CodeBadPC)
	errs, err := viewConn.UpdateSubscriptions(ctx, []sfu.SubscriptionUpdate{{Share: "s_2", Video: sfu.QualityLow}})
	if err != nil {
		t.Fatal(err)
	}
	wantCode(t, errs[0], sfu.CodeInternal)
	if v, a, ok := h.SFU.FanOut("s_2"); !ok || v != 0 || a != 0 {
		t.Errorf("FanOut(s_2) after the refused subscription = %d, %d, %v; want nothing left behind", v, a, ok)
	}
	// The share ends: the subscription goes, without an offer on the closed PC and without an error event.
	if err := pubConn.StopShare(ctx, "s_1", sfu.EndReasonStopped); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		subs, err := viewConn.Subscriptions(ctx)
		return err == nil && len(subs) == 0
	}, func() string { return "the subscription to the ended share is still there" })
	time.Sleep(200 * time.Millisecond) // four debounce periods
	if n := len(sig.Offers()); n != 1 {
		t.Errorf("%d sub offers, want only the first: nothing is offered on a closed PC", n)
	}
	for _, ev := range sig.Events() {
		if _, ok := ev.(sfu.ErrorEvent); ok {
			t.Errorf("the viewer got %+v", ev)
		}
	}
	if _, err := viewConn.Subscriptions(ctx); err != nil {
		t.Errorf("the Conn after its sub PC closed: %v", err)
	}
}

// TestSubAnswerRefusedByPion: an answer that passes the SFU's checks but that Pion refuses after taking it is fatal
// for the sub PC (02 §5.3, the closed row): Pion has no rollback, so the right answer could not be applied either.
// The SFU closes the PC and offers nothing more on it; the Conn and its subscriptions stay for the rebuild, which is
// README S57's. An answer the SFU's own checks refuse leaves the offer outstanding (TestSubNegotiation).
func TestSubAnswerRefusedByPion(t *testing.T) {
	h := newHarness(t, sfutest.HarnessOptions{})
	ctx := testCtx(t)
	pubConn, _ := join(t, h, "alice", "c-pub", sfu.RoleFull)
	viewConn, sig := join(t, h, "bob", "c-view", sfu.RoleViewer)
	startShare(t, pubConn, "s_1", sfu.PresetAuto)
	startShare(t, pubConn, "s_2", sfu.PresetAuto)
	viewer := newViewer(t, sfutest.LoopbackSettings()) // answered by hand: no Attach
	subscribe := func(id sfu.ShareID) error {
		t.Helper()
		errs, err := viewConn.UpdateSubscriptions(ctx, []sfu.SubscriptionUpdate{{Share: id, Video: sfu.QualityLow}})
		if err != nil {
			t.Fatal(err)
		}
		return errs[0]
	}
	if err := subscribe("s_1"); err != nil {
		t.Fatal(err)
	}
	offer, err := sig.WaitOffer(ctx, func(o sfutest.Offer) bool { return o.Neg == 1 })
	if err != nil {
		t.Fatal(err)
	}
	answer, err := viewer.Answer(ctx, webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer.SDP})
	if err != nil {
		t.Fatal(err)
	}

	// The answer lost its ICE user name, which Pion looks for only after it has taken the answer.
	wantCode(t, viewConn.HandleAnswer(ctx, sfu.PCSub, 1, 1, withoutLines(answer.SDP, "a=ice-ufrag:")), sfu.CodeBadSDP)
	// The PC is closed, in the same call: the right answer finds nothing to apply to.
	wantCode(t, viewConn.HandleAnswer(ctx, sfu.PCSub, 1, 1, answer.SDP), sfu.CodeBadPC)
	if err := sig.WaitPCState(ctx, sfu.PCSub, 1, "closed"); err != nil {
		t.Fatalf("%v (events %+v)", err, sig.Events())
	}

	// Like a sub PC the client closed: the subscription stays, a new one fails until the rebuild exists, and nothing
	// more is offered.
	wantCode(t, subscribe("s_2"), sfu.CodeInternal)
	time.Sleep(200 * time.Millisecond) // four debounce periods
	if n := len(sig.Offers()); n != 1 {
		t.Errorf("%d sub offers, want only the first: nothing is offered on a closed PC", n)
	}
	subs, err := viewConn.Subscriptions(ctx)
	if err != nil || len(subs) != 1 || subs[0].Share != "s_1" {
		t.Errorf("subscriptions after the sub PC closed = %+v, %v; want the one to s_1 kept for the rebuild", subs, err)
	}
	for _, ev := range sig.Events() {
		if st, ok := ev.(sfu.PCStateEvent); !ok || st.PC != sfu.PCSub || st.Gen != 1 || st.State != "closed" {
			t.Errorf("the viewer got %+v, want only the closed state: HandleAnswer returned the error", ev)
		}
	}
}

// TestMediaOverTCP443: with UDP off and no 7882/tcp, both PCs connect through the 443 multiplexer's ICE side: the
// clients dial the passive TCP 443 candidate of the pub answer and the sub offer.
func TestMediaOverTCP443(t *testing.T) {
	h := newHarness(t, sfutest.HarnessOptions{NoUDP: true, TCP443: true})
	ctx := testCtx(t)
	pubConn, pubSig := join(t, h, "alice", "c-pub", sfu.RoleFull)
	viewConn, viewSig := join(t, h, "bob", "c-view", sfu.RoleViewer)
	const share = sfu.ShareID("s_tcp")
	startShare(t, pubConn, share, sfu.PresetAuto)
	only443 := []cand{{
		proto: "tcp", addr: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), h.TCP443Port()), typ: "host", passive: true,
	}}

	pub := newPublisher(t, newSource(t), sfutest.LoopbackTCPSettings())
	answer, err := sfutest.Publish(ctx, pubConn, pub, 1, 1, share)
	if err != nil {
		t.Fatal(err)
	}
	if got := candidates(t, answer); !slices.Equal(got, only443) {
		t.Errorf("pub answer candidates = %+v, want only %+v", got, only443)
	}
	if err := pub.Start(ctx); err != nil {
		t.Fatal(err)
	}

	viewer := newViewer(t, sfutest.LoopbackTCPSettings())
	viewSig.Attach(viewConn, viewer)
	if _, err := viewConn.UpdateSubscriptions(ctx, []sfu.SubscriptionUpdate{{Share: share, Video: sfu.QualityHigh, Audio: true}}); err != nil {
		t.Fatal(err)
	}
	offer, err := viewSig.WaitOffer(ctx, func(sfutest.Offer) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if got := candidates(t, offer.SDP); !slices.Equal(got, only443) {
		t.Errorf("sub offer candidates = %+v, want only %+v", got, only443)
	}
	if vias := checkServerCandidates(t, h, "the sub offer", offer.SDP); !slices.Equal(vias, []string{netx.ViaTCP443}) {
		t.Errorf("the sub offer's candidates are on %v, want only tcp443", vias)
	}

	if err := pubSig.WaitPCState(ctx, sfu.PCPub, 1, "connected"); err != nil {
		t.Fatalf("pub PC over TCP 443: %v", err)
	}
	if err := viewSig.WaitPCState(ctx, sfu.PCSub, 1, "connected"); err != nil {
		t.Fatalf("sub PC over TCP 443: %v (answer errors %v)", err, viewSig.Errs())
	}
	waitShare(t, h.SFU, share, func(i sfu.ShareInfo) bool { return len(i.Layers) == 2 && i.Audio })
	// Both clients came in through the multiplexer's ICE side, and their selected pairs end on its port.
	if st := h.PortMux.Stats(); st.ICE < 2 || st.Garbage != 0 || st.Limited != 0 {
		t.Errorf("443 multiplexer stats = %+v, want at least two ICE connections and nothing refused", st)
	}
	for name, pc := range map[string]*webrtc.PeerConnection{"publisher": pub.PC(), "viewer": viewer.PC()} {
		pair, err := pc.SCTP().Transport().ICETransport().GetSelectedCandidatePair()
		if err != nil || pair == nil {
			t.Errorf("%s: no selected candidate pair: %v", name, err)
			continue
		}
		if pair.Remote.Protocol != webrtc.ICEProtocolTCP || pair.Remote.Port != h.TCP443Port() {
			t.Errorf("%s: selected remote candidate %s %s, want TCP on port %d", name, pair.Remote.Protocol,
				net.JoinHostPort(pair.Remote.Address, fmt.Sprint(pair.Remote.Port)), h.TCP443Port())
		}
	}
}
