package sfu_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/client/publish"
	"github.com/MoonWX/isshoni/internal/media/fake"
	"github.com/MoonWX/isshoni/internal/server/sfu"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
)

// The acceptance tests of the codec policy and of viewers without H.264 (README S69): integration 12 and 13 of
// 02 §17, through sfutest.Harness with real PeerConnections and fake media.

// The H.264 profile-level-ids of a Firefox that has fetched OpenH264: Constrained Baseline and Baseline.
var firefoxH264 = []string{"42e01f", "42001f"}

// joinDecoding joins a Conn whose caps decode the given H.264 profiles (none: a fresh Firefox).
func joinDecoding(t *testing.T, h *sfutest.Harness, user sfu.UserID, conn sfu.ConnID, role sfu.Role,
	caps ...sfu.ProfileKey,
) (*sfu.Conn, *sfutest.DirectSignaler) {
	t.Helper()
	c, sig, err := h.Join(sfu.JoinParams{
		Room: "lounge", Participant: sfu.ParticipantID(user), User: user, Conn: conn, Role: role,
		Decode: sfu.DecodeCaps{H264: caps},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, sig
}

// newViewerWith is newViewer on loopback with options of its own; it closes the Viewer when the test ends.
func newViewerWith(t *testing.T, o sfutest.ViewerOptions) *sfutest.Viewer {
	t.Helper()
	o.Settings = sfutest.LoopbackSettings()
	v, err := sfutest.NewViewer(o)
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

// profileSource returns a ProfileSource of small fake media (f and q with audio, a GOP far longer than a test, so
// that every keyframe after the first is one somebody asked for), starting in profile.
func profileSource(t *testing.T, profile string, seed uint64) *sfutest.ProfileSource {
	t.Helper()
	src, err := sfutest.NewProfileSource(profile, func(p string) (*fake.Source, error) {
		return fake.New(fake.Config{
			Profile: p,
			Layers: []fake.VideoLayer{
				{RID: "f", Width: 640, Height: 360, FPS: 30, Bitrate: 400_000},
				{RID: "q", Width: 320, Height: 180, FPS: 15, Bitrate: 100_000},
			},
			GOP: time.Minute, Audio: true, Seed: seed,
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	return src
}

// codecHints returns the profiles of the CodecPolicyEvents a Conn's client got about a share, in order.
func codecHints(sig *sfutest.DirectSignaler, share sfu.ShareID) []sfu.ProfileKey {
	var out []sfu.ProfileKey
	for _, ev := range sig.Events() {
		if h, ok := ev.(sfu.CodecPolicyEvent); ok && h.Share == share {
			out = append(out, h.Profile)
		}
	}
	return out
}

// waitHint waits until the newest CodecPolicyEvent about a share has the profile.
func waitHint(t *testing.T, sig *sfutest.DirectSignaler, share sfu.ShareID, want sfu.ProfileKey) {
	t.Helper()
	eventually(t, func() bool {
		hints := codecHints(sig, share)
		return len(hints) > 0 && hints[len(hints)-1] == want
	}, func() string {
		return fmt.Sprintf("codec hints about %s: %v, want the last %s", share, codecHints(sig, share), want)
	})
}

// answerVideoProfiles returns the H.264 profiles of a pub answer's video m-section, in its order.
func answerVideoProfiles(t *testing.T, answer string) []string {
	t.Helper()
	for _, md := range parse(t, answer).MediaDescriptions {
		if md.MediaName.Media != "video" {
			continue
		}
		var out []string
		for _, f := range md.MediaName.Formats {
			for _, a := range md.Attributes {
				if a.Key == "fmtp" && strings.HasPrefix(a.Value, f+" ") {
					if _, plid, ok := strings.Cut(a.Value, "profile-level-id="); ok && len(plid) >= 4 {
						out = append(out, plid[:4])
					}
				}
			}
		}
		return out
	}
	return nil
}

// videoPTsSince returns the payload types of a Recorder's packets from index skip on, in sequence-number order, each
// run of one PT once. (Read in arrival order, a repair of an older packet that came late would show its PT again.)
func videoPTsSince(r *sfutest.Recorder, skip int) []uint8 {
	var out []uint8
	pkts := r.Packets()
	for _, p := range sfutest.InSeqOrder(pkts[min(skip, len(pkts)):]) {
		if len(out) == 0 || out[len(out)-1] != p.PT {
			out = append(out, p.PT)
		}
	}
	return out
}

// waitPT waits until a Recorder has n packets with payload type pt after its first skip.
func waitPT(t *testing.T, r *sfutest.Recorder, skip int, pt uint8, n int) {
	t.Helper()
	err := r.Wait(testCtx(t), func(r *sfutest.Recorder) bool {
		got := 0
		pkts := r.Packets()
		for _, p := range pkts[min(skip, len(pkts)):] {
			if p.PT == pt {
				got++
			}
		}
		return got >= n
	})
	if err != nil {
		t.Fatalf("%d packets with PT %d after packet %d: %v (PTs %v)", n, pt, skip, err, videoPTsSince(r, skip))
	}
}

// checkAcrossProfiles is checkVideo for a stream whose profile changes: one continuous stream that starts on an SPS,
// as does every change of layer and every change of payload type, the profile's. The frame checks run on each run of
// one payload type alone, because the sharer's fake media starts its frame count again with the new profile's
// Source (sfutest.ProfileSource).
func checkAcrossProfiles(t *testing.T, what string, pkts []sfutest.Packet) {
	t.Helper()
	pkts = sfutest.WithoutPartialTail(pkts)
	for _, check := range []func([]sfutest.Packet) error{sfutest.CheckContinuous, sfutest.CheckStartsOnSPS} {
		if err := check(pkts); err != nil {
			t.Errorf("%s: %v", what, err)
		}
	}
	in := sfutest.InSeqOrder(pkts)
	for start, i := 0, 1; i <= len(in); i++ {
		if i < len(in) && in[i].PT == in[start].PT {
			continue
		}
		run := in[start:i]
		if !run[0].KeyStart {
			t.Errorf("%s: payload type %d starts at seq %d without an SPS", what, run[0].PT, run[0].Seq)
		}
		if err := sfutest.CheckVideoMarkers(sfutest.WithoutCutFrames(run)); err != nil {
			t.Errorf("%s, payload type %d from seq %d: %v", what, run[0].PT, run[0].Seq, err)
		}
		start = i
	}
}

// publisherProfiles returns the profile and payload type of each video layer of a Publisher, "f/6400/96 q/6400/96".
func publisherProfiles(pub *publish.Publisher) string {
	var out []string
	for _, ts := range pub.Stats().Tracks {
		if ts.Kind == webrtc.RTPCodecTypeVideo {
			out = append(out, fmt.Sprintf("%s/%s/%d", ts.Layer, ts.Profile, ts.PT))
		}
	}
	return strings.Join(out, " ")
}

// TestCodecPolicy is integration 12 of 02 §17. A sharer sends High to a Chrome viewer. A viewer that decodes only
// Constrained Baseline joins: the room's policy is cb at once, the sharer gets a CodecPolicyEvent{42e0}, the new
// viewer hears codec_mismatch while the share is still High, the sharer re-offers with 42e0 first, the answer holds
// only 42e0 and 4200 (02 §8.4 step 3: Pion honours the transceiver's codec preferences in its answers), and both
// viewers then get the video on their Constrained Baseline payload type, the Chrome viewer without a gap in its
// stream. When the Firefox viewer has left, the policy goes back to high 60 s later on the fake clock, and the
// sharer back to High.
func TestCodecPolicy(t *testing.T) {
	h := newHarness(t, sfutest.HarnessOptions{})
	ctx := testCtx(t)
	video := webrtc.RTPCodecTypeVideo
	const share = sfu.ShareID("s_policy")

	// A sharer that can encode High and Constrained Baseline, like Chrome, and starts with High.
	pubConn, pubSig := join(t, h, "alice", "c-alice", sfu.RoleFull)
	startShare(t, pubConn, share, sfu.PresetAuto)
	src := profileSource(t, "6400", 12)
	pub, err := publish.New(publish.Options{
		Source: src, Settings: sfutest.LoopbackSettings(), Audio: true, Profile: "6400", Profiles: []string{"42e0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pub.Close() })
	answer, err := sfutest.Publish(ctx, pubConn, pub, 1, 1, share)
	if err != nil {
		t.Fatal(err)
	}
	if got := answerVideoProfiles(t, answer); !slices.Equal(got, []string{"6400", "42e0"}) {
		t.Errorf("the first answer offers %v, want the sharer's own order under the high policy", got)
	}
	if err := pub.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitShare(t, h.SFU, share, func(i sfu.ShareInfo) bool { return i.State == sfu.ShareLive && i.Profile == sfu.ProfileHigh })

	// A Chrome viewer gets High on its High payload type (104 in sfutest.DefaultH264's order).
	chromeConn, chromeSig := join(t, h, "bob", "c-chrome", sfu.RoleViewer)
	chrome := newViewer(t, sfutest.LoopbackSettings())
	chromeSig.Attach(chromeConn, chrome)
	subscribe(t, chromeConn, sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityHigh, Audio: true})
	chromeVideo := streamTrack(t, chrome, share, video)
	waitPT(t, chromeVideo, 0, 104, 30)
	if len(codecHints(pubSig, share)) != 0 {
		t.Errorf("hints before any change: %v", codecHints(pubSig, share))
	}

	// ---- a Firefox viewer joins: cb at once ----
	ffConn, ffSig := joinDecoding(t, h, "carol", "c-firefox", sfu.RoleViewer,
		sfu.ProfileConstrainedBaseline, sfu.ProfileBaseline)
	if p := h.SFU.CodecPolicy("lounge"); p != sfu.ProfileConstrainedBaseline {
		t.Fatalf("policy %s after a Firefox viewer joined, want cb at once", p)
	}
	if _, err := h.Events.Wait(ctx, func(ev sfutest.RoomEvent) bool {
		return ev.Kind == sfutest.CodecPolicyChanged && ev.Policy == sfu.ProfileConstrainedBaseline
	}); err != nil {
		t.Fatal(err)
	}
	waitHint(t, pubSig, share, sfu.ProfileConstrainedBaseline)

	// Before the sharer has switched, the Firefox viewer can't decode the stream: codec_mismatch.
	ff := newViewerWith(t, sfutest.ViewerOptions{H264: firefoxH264})
	ffSig.Attach(ffConn, ff)
	subscribe(t, ffConn, sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityHigh, Audio: true})
	waitSubState(t, ffSig, sfu.SubscriptionStateEvent{
		Share: share, Requested: sfu.QualityHigh, Forwarded: sfu.QualityOff, Audio: true, Reason: sfu.SubReasonCodecMismatch,
	})
	if subs, err := ffConn.Subscriptions(ctx); err != nil || len(subs) != 1 || subs[0].VideoSent != 0 ||
		!samePTs(subs[0].VideoPTs, map[sfu.ProfileKey]uint8{"42e0": 96, "4200": 98}) {
		t.Fatalf("the Firefox viewer's subscription = %+v, %v; want CB payload types and nothing sent", subs, err)
	}

	// ---- the sharer follows the hint: 42e0 first, re-offer ----
	if err := pub.SetProfile("42e0"); err != nil {
		t.Fatal(err)
	}
	chromeBefore := len(chromeVideo.Packets())
	answer, err = sfutest.Publish(ctx, pubConn, pub, 1, 2, share)
	if err != nil {
		t.Fatal(err)
	}
	if got := answerVideoProfiles(t, answer); !slices.Equal(got, []string{"42e0"}) {
		t.Fatalf("the answer under cb offers %v, want only 42e0 (with 4200 if offered)", got)
	}
	eventually(t, func() bool { return publisherProfiles(pub) == "f/42e0/98 q/42e0/98" },
		func() string { return "the sharer sends " + publisherProfiles(pub) })
	if p, n := src.Profile(); p != "42e0" || n != 1 {
		t.Errorf("the source encodes %s after %d switches, want 42e0 after 1", p, n)
	}
	waitShare(t, h.SFU, share, func(i sfu.ShareInfo) bool { return i.Profile == sfu.ProfileConstrainedBaseline })

	// Both viewers get it on their CB payload type: 96 for both.
	ffVideo := streamTrack(t, ff, share, video)
	waitPT(t, ffVideo, 0, 96, 30)
	waitFrames(t, ffVideo, 0, "f", 10)
	checkVideo(t, "the Firefox viewer's video", ffVideo.Packets())
	waitSubState(t, ffSig, sfu.SubscriptionStateEvent{
		Share: share, Requested: sfu.QualityHigh, Forwarded: sfu.QualityHigh, Audio: true,
	})
	waitPT(t, chromeVideo, chromeBefore, 96, 30)
	if pts := videoPTsSince(chromeVideo, 0); !slices.Equal(pts, []uint8{104, 96}) {
		t.Errorf("the Chrome viewer's payload types run %v, want 104 then 96", pts)
	}
	// The change of profile is one more start of a stream on a keyframe in the Chrome viewer's one sequence.
	checkAcrossProfiles(t, "the Chrome viewer's video across the switch", chromeVideo.Packets())
	if slices.ContainsFunc(subEvents(chromeSig, share), func(ev sfu.SubscriptionStateEvent) bool {
		return ev.Reason != sfu.SubReasonNone
	}) {
		t.Errorf("the Chrome viewer heard a reason: %+v", subEvents(chromeSig, share))
	}

	// ---- the Firefox viewer leaves: high again 60 s later, on the fake clock ----
	// (10 s short of the hold: room for the real time a loaded machine takes between the steps.)
	ffConn.Close(sfu.EndReasonLeft)
	h.SFU.CheckCodecPolicies()
	h.SFU.AdvanceCodecClock(sfu.CodecPolicyHold - 10*time.Second)
	h.SFU.CheckCodecPolicies()
	if p := h.SFU.CodecPolicy("lounge"); p != sfu.ProfileConstrainedBaseline {
		t.Fatalf("policy %s 50 s after the Firefox viewer left, want cb", p)
	}
	h.SFU.AdvanceCodecClock(10 * time.Second)
	h.SFU.CheckCodecPolicies()
	if p := h.SFU.CodecPolicy("lounge"); p != sfu.ProfileHigh {
		t.Fatalf("policy %s 60 s after the Firefox viewer left, want high", p)
	}
	waitHint(t, pubSig, share, sfu.ProfileHigh)
	if hints := codecHints(pubSig, share); !slices.Equal(hints, []sfu.ProfileKey{"42e0", "6400"}) {
		t.Errorf("the sharer's hints %v, want 42e0 then 6400", hints)
	}
	if err := pub.SetProfile("6400"); err != nil {
		t.Fatal(err)
	}
	chromeBefore = len(chromeVideo.Packets())
	answer, err = sfutest.Publish(ctx, pubConn, pub, 1, 3, share)
	if err != nil {
		t.Fatal(err)
	}
	if got := answerVideoProfiles(t, answer); !slices.Equal(got, []string{"6400", "42e0"}) {
		t.Errorf("the answer back under high offers %v, want High first", got)
	}
	waitPT(t, chromeVideo, chromeBefore, 104, 30)
	checkAcrossProfiles(t, "the Chrome viewer's video back on High", chromeVideo.Packets())
	if p, n := src.Profile(); p != "6400" || n != 2 {
		t.Errorf("the source encodes %s after %d switches, want 6400 after 2", p, n)
	}
}

// samePTs reports whether two payload type maps are equal.
func samePTs(a, b map[sfu.ProfileKey]uint8) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// cbShare starts a share on a new Conn of user and publishes Constrained Baseline media on it, simulcast f and q with
// audio, and waits until it is live.
func cbShare(t *testing.T, h *sfutest.Harness, user sfu.UserID, conn sfu.ConnID, share sfu.ShareID, seed uint64) *sfu.Conn {
	t.Helper()
	ctx := testCtx(t)
	c, _ := join(t, h, user, conn, sfu.RoleFull)
	startShare(t, c, share, sfu.PresetAuto)
	pub, err := publish.New(publish.Options{
		Source: profileSource(t, "42e0", seed), Settings: sfutest.LoopbackSettings(), Audio: true, Profile: "42e0",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pub.Close() })
	if _, err := sfutest.Publish(ctx, c, pub, 1, 1, share); err != nil {
		t.Fatal(err)
	}
	if err := pub.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitLive(t, h, share)
	return c
}

// waitDecoders waits until a Conn's DecoderState satisfies cond and returns it.
func waitDecoders(t *testing.T, c *sfu.Conn, cond func(sfu.DecoderState) bool) sfu.DecoderState {
	t.Helper()
	var st sfu.DecoderState
	eventually(t, func() bool {
		var err error
		st, err = c.Decoders(testCtx(t))
		return err == nil && cond(st)
	}, func() string { return fmt.Sprintf("decoder state %+v", st) })
	return st
}

// subGen returns the gen of a Conn's sub PC.
func subGen(t *testing.T, c *sfu.Conn) uint32 {
	t.Helper()
	return pcOf(t, c, sfu.PCSub).Gen
}

// TestDecoderUnavailable is integration 13 of 02 §17: viewers without H.264 (a fresh Firefox profile, S4).
//
// Caps-driven: a viewer whose caps list no H.264 gets the audio and no video m-section, and hears
// decoder_unavailable; when its caps gain H.264 (caps.update) its sub PC is rebuilt with the video, and the video
// flows. The guard: a viewer whose caps list H.264 but whose answer refuses it (its MediaEngine has none) doesn't
// fail the negotiation, its audio flows, and the SFU rebuilds its sub PC every 20 s (shortened here), 9 times, and
// then reports decoder_failed and stops. None of those rebuilds counts toward the PC creation limit. A caps.update
// after its decoder download starts again and gets the video.
func TestDecoderUnavailable(t *testing.T) {
	if d := sfu.DefaultTimings(); d.CodecRetry != 20*time.Second || sfu.CodecRetries != 9 || sfu.CodecPolicyHold != time.Minute {
		t.Errorf("codec retries every %v, %d of them, policy hold %v; want the 20 s × 9 and the 60 s of 02 §8.3, §8.5",
			d.CodecRetry, sfu.CodecRetries, sfu.CodecPolicyHold)
	}
	const retry = 300 * time.Millisecond
	h := newHarness(t, sfutest.HarnessOptions{})
	h.SFU.SetTimings(sfu.Timings{CodecRetry: retry})
	ctx := ctxFor(t, 90*time.Second)
	video, audio := webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio
	const share = sfu.ShareID("s_firefox")

	// A sharer in Constrained Baseline, which every viewer here decodes once it has a decoder.
	cbShare(t, h, "alice", "c-alice", share, 13)

	t.Run("caps-driven", func(t *testing.T) {
		// A fresh Firefox: no H.264 in its caps, nor in its MediaEngine.
		c, sig := joinDecoding(t, h, "dan", "c-fresh", sfu.RoleViewer)
		v := newViewerWith(t, sfutest.ViewerOptions{NoH264: true})
		sig.Attach(c, v)
		subscribe(t, c, sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityHigh, Audio: true})
		offer, err := sig.WaitOffer(ctx, func(o sfutest.Offer) bool { return o.Gen == 1 })
		if err != nil {
			t.Fatal(err)
		}
		if len(offer.Tracks) != 1 || offer.Tracks[0].Kind != audio || len(sectionsOf(t, offer.SDP)) != 1 {
			t.Fatalf("the offer to a viewer without H.264 carries %+v, want its audio alone", offer.Tracks)
		}
		waitSubState(t, sig, sfu.SubscriptionStateEvent{
			Share: share, Requested: sfu.QualityHigh, Forwarded: sfu.QualityOff, Audio: true,
			Reason: sfu.SubReasonDecoderUnavailable,
		})
		heard := streamTrack(t, v, share, audio)
		if err := heard.Wait(ctx, func(r *sfutest.Recorder) bool { return r.Stats().Packets >= 50 }); err != nil {
			t.Fatalf("the audio of a viewer without H.264: %v", err)
		}

		// The decoder download is done: the browser's new PeerConnections decode CB, and the client says so.
		if err := v.SetH264(firefoxH264); err != nil {
			t.Fatal(err)
		}
		if err := c.SetDecodeCaps(ctx, sfu.DecodeCaps{H264: []sfu.ProfileKey{"42e0", "4200"}}); err != nil {
			t.Fatal(err)
		}
		rebuilt, err := sig.WaitOffer(ctx, func(o sfutest.Offer) bool { return o.Gen == 2 })
		if err != nil {
			t.Fatalf("no new sub PC after the viewer gained H.264: %v", err)
		}
		if len(rebuilt.Tracks) != 2 || rebuilt.Neg != 1 {
			t.Errorf("the rebuilt sub PC's first offer carries %+v (neg %d), want video and audio", rebuilt.Tracks, rebuilt.Neg)
		}
		seen := streamTrack(t, v, share, video)
		waitPT(t, seen, 0, 96, 30)
		waitFrames(t, seen, 0, "f", 10)
		checkVideo(t, "video after the decoder download", seen.Packets())
		waitSubState(t, sig, sfu.SubscriptionStateEvent{
			Share: share, Requested: sfu.QualityHigh, Forwarded: sfu.QualityHigh, Audio: true,
		})
		if st := waitDecoders(t, c, func(sfu.DecoderState) bool { return true }); !st.H264 || st.Refused || st.Rebuilds != 0 {
			t.Errorf("decoder state %+v after the video came", st)
		}
		if g := subGen(t, c); g != 2 {
			t.Errorf("sub gen %d, want 2: one rebuild for the decoder", g)
		}
	})

	t.Run("guard", func(t *testing.T) {
		// Caps that list H.264 too early: the MediaEngine has none yet, so every answer refuses the video.
		c, sig := joinDecoding(t, h, "erin", "c-early", sfu.RoleViewer, sfu.ProfileConstrainedBaseline)
		v := newViewerWith(t, sfutest.ViewerOptions{NoH264: true})
		sig.Attach(c, v)
		subscribe(t, c, sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityHigh, Audio: true})
		first, err := sig.WaitOffer(ctx, func(o sfutest.Offer) bool { return o.Gen == 1 })
		if err != nil {
			t.Fatal(err)
		}
		if len(first.Tracks) != 2 {
			t.Fatalf("the first offer carries %+v, want video and audio: the caps list H.264", first.Tracks)
		}
		// No negotiation failure: the answer is applied, and the video waits for a decoder.
		start := time.Now()
		if err := sig.WaitAnswered(ctx, first); err != nil {
			t.Fatal(err)
		}
		if errs := sig.Errs(); len(errs) != 0 {
			t.Fatalf("the answer that refuses the video failed: %v", errs)
		}
		eventually(t, func() bool {
			return slices.ContainsFunc(subEvents(sig, share), func(ev sfu.SubscriptionStateEvent) bool {
				return ev.Reason == sfu.SubReasonDecoderUnavailable && ev.Forwarded == sfu.QualityOff
			})
		}, func() string {
			return fmt.Sprintf("the client's events %+v, want decoder_unavailable", subEvents(sig, share))
		})

		// Nine codec rebuilds, one per interval, each answered with the video refused again; then decoder_failed.
		st := waitDecoders(t, c, func(st sfu.DecoderState) bool { return st.Failed })
		took := time.Since(start)
		if st.Rebuilds != sfu.CodecRetries || st.Retry || !st.Refused {
			t.Errorf("decoder state %+v once failed, want %d rebuilds and no retry set", st, sfu.CodecRetries)
		}
		if g := subGen(t, c); g != 1+sfu.CodecRetries {
			t.Errorf("sub gen %d after the retries, want %d", g, 1+sfu.CodecRetries)
		}
		if took < (sfu.CodecRetries-1)*retry {
			t.Errorf("the %d rebuilds took %v, want one per %v", sfu.CodecRetries, took, retry)
		}
		// The last PC stays: its audio flows, and its video is decoder_failed.
		waitSubState(t, sig, sfu.SubscriptionStateEvent{
			Share: share, Requested: sfu.QualityHigh, Forwarded: sfu.QualityOff, Audio: true,
			Reason: sfu.SubReasonDecoderFailed,
		})
		heard := streamTrack(t, v, share, audio)
		if err := heard.Wait(ctx, func(r *sfutest.Recorder) bool { return r.Stats().Packets >= 25 }); err != nil {
			t.Fatalf("the audio of a viewer whose answers refuse the video: %v", err)
		}
		for _, ev := range subEvents(sig, share) {
			if ev.Reason != sfu.SubReasonDecoderUnavailable && ev.Reason != sfu.SubReasonDecoderFailed {
				t.Errorf("the client heard %+v while the guard ran", ev)
			}
		}
		time.Sleep(4 * retry)
		if g := subGen(t, c); g != 1+sfu.CodecRetries {
			t.Errorf("sub gen %d after decoder_failed, want no more rebuilds", g)
		}
		if errs := sig.Errs(); len(errs) != 0 {
			t.Fatalf("the rebuilt PCs' answers failed: %v", errs)
		}

		// Caps that say what they said (a caps.update about something else) change nothing.
		if err := c.SetDecodeCaps(ctx, sfu.DecodeCaps{H264: []sfu.ProfileKey{sfu.ProfileConstrainedBaseline}}); err != nil {
			t.Fatal(err)
		}
		if st := waitDecoders(t, c, func(sfu.DecoderState) bool { return true }); !st.Failed || subGen(t, c) != 1+sfu.CodecRetries {
			t.Errorf("the same caps again: %+v at gen %d, want decoder_failed held and no rebuild", st, subGen(t, c))
		}

		// The SFU's own rebuilds don't count toward the limit on PC creations (02 §12): ten within this minute, and
		// the client's own rebuild still gets a PC.
		if err := c.ResetPC(ctx, sfu.PCSub, 1+sfu.CodecRetries); err != nil {
			t.Fatalf("ResetPC after %d codec rebuilds: %v", sfu.CodecRetries, err)
		}
		gen := uint32(2 + sfu.CodecRetries)
		if _, err := sig.WaitOffer(ctx, func(o sfutest.Offer) bool { return o.Gen == gen }); err != nil {
			t.Fatal(err)
		}
		time.Sleep(4 * retry)
		if st := waitDecoders(t, c, func(sfu.DecoderState) bool { return true }); !st.Failed || st.Retry || subGen(t, c) != gen {
			t.Errorf("after the client's rebuild: %+v at gen %d, want decoder_failed held", st, subGen(t, c))
		}

		// The decoder download is done and the client says so: the guard starts again, at once, and the video comes.
		if err := v.SetH264(firefoxH264); err != nil {
			t.Fatal(err)
		}
		if err := c.SetDecodeCaps(ctx, sfu.DecodeCaps{H264: []sfu.ProfileKey{"42e0", "4200"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := sig.WaitOffer(ctx, func(o sfutest.Offer) bool { return o.Gen == gen+1 }); err != nil {
			t.Fatalf("no codec rebuild after the caps changed: %v", err)
		}
		seen := streamTrack(t, v, share, video)
		waitPT(t, seen, 0, 96, 30)
		checkVideo(t, "video after the guard", seen.Packets())
		waitSubState(t, sig, sfu.SubscriptionStateEvent{
			Share: share, Requested: sfu.QualityHigh, Forwarded: sfu.QualityHigh, Audio: true,
		})
		if st := waitDecoders(t, c, func(st sfu.DecoderState) bool { return !st.Refused }); st.Failed || st.Rebuilds != 0 || st.Retry {
			t.Errorf("decoder state %+v with the video flowing", st)
		}
	})

	t.Run("decoder between shares", func(t *testing.T) {
		// A fresh Firefox watches a share that ends: its sub PC, negotiated without a decoder, carries nothing.
		const brief = sfu.ShareID("s_brief")
		briefConn := cbShare(t, h, "gina", "c-gina", brief, 14)
		c, sig := joinDecoding(t, h, "frank", "c-between", sfu.RoleViewer)
		v := newViewerWith(t, sfutest.ViewerOptions{NoH264: true})
		sig.Attach(c, v)
		subscribe(t, c, sfu.SubscriptionUpdate{Share: brief, Video: sfu.QualityHigh, Audio: true})
		first, err := sig.WaitOffer(ctx, func(o sfutest.Offer) bool { return o.Gen == 1 })
		if err != nil {
			t.Fatal(err)
		}
		if err := sig.WaitAnswered(ctx, first); err != nil {
			t.Fatal(err)
		}
		if err := briefConn.StopShare(ctx, brief, sfu.EndReasonStopped); err != nil {
			t.Fatal(err)
		}
		gone, err := sig.WaitOffer(ctx, func(o sfutest.Offer) bool { return o.Gen == 1 && o.Neg == 2 })
		if err != nil {
			t.Fatal(err)
		}
		if err := sig.WaitAnswered(ctx, gone); err != nil {
			t.Fatal(err)
		}

		// Its decoder arrives while it watches nothing: no PC is rebuilt for nothing, but the next subscription
		// gets a new one instead of video on the old one, which the browser might not decode on.
		if err := v.SetH264(firefoxH264); err != nil {
			t.Fatal(err)
		}
		if err := c.SetDecodeCaps(ctx, sfu.DecodeCaps{H264: []sfu.ProfileKey{"42e0", "4200"}}); err != nil {
			t.Fatal(err)
		}
		if st := pcOf(t, c, sfu.PCSub); st.Gen != 1 || st.Closed || st.Offering {
			t.Errorf("the sub PC without subscriptions after the caps changed: %+v, want gen 1 as it was", st)
		}
		subscribe(t, c, sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityHigh, Audio: true})
		next, err := sig.WaitOffer(ctx, func(o sfutest.Offer) bool { return o.Gen == 2 })
		if err != nil {
			t.Fatalf("no new sub PC for the first subscription after the decoder came: %v", err)
		}
		if next.Neg != 1 || len(next.Tracks) != 2 {
			t.Errorf("the new sub PC's offer has neg %d and %+v, want video and audio", next.Neg, next.Tracks)
		}
		if slices.ContainsFunc(sig.Offers(), func(o sfutest.Offer) bool { return o.Gen == 1 && o.Neg > 2 }) {
			t.Errorf("the old sub PC was renegotiated: %+v", sig.Offers())
		}
		seen := streamTrack(t, v, share, video)
		waitPT(t, seen, 0, 96, 30)
		checkVideo(t, "video on the new sub PC", seen.Packets())
	})
}
