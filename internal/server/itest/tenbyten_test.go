package itest

import (
	"cmp"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/servertest"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
)

// TestTenByTen is case 9 of 01 §19 (from S4; skipped with -short): ten members share, each a simulcast share with
// audio, and ten more watch all ten shares, each with another share in focus (the full layer, with audio) and the
// other nine as thumbnails (the preview, without). Twenty accounts, twenty WebSockets and twenty PeerConnections
// through one server: all hundred video subscriptions get the layer they asked for, audio flows only on the
// focused shares, and room.state tells everyone who watches what.
func TestTenByTen(t *testing.T) {
	if testing.Short() {
		t.Skip("10×10 runs outside -short")
	}
	const n = 10
	w := newWorld(t, servertest.Options{})

	sharers := make([]*client, n)
	shares := make([]*share, n)
	for i := range n {
		sharers[i] = w.connect(w.member(fmt.Sprintf("sharer%d", i)), protocol.RoleFull)
		shares[i] = sharers[i].publish(uint64(100+i), sfutest.LoopbackSettings())
	}
	viewers := make([]*client, n)
	recorders := make([]*sfutest.Viewer, n)
	for j := range n {
		viewers[j] = w.connect(w.member(fmt.Sprintf("viewer%d", j)), protocol.RoleViewer)
		recorders[j] = viewers[j].watch(sfutest.LoopbackSettings(), 1<<13)
	}
	// Every viewer sees all ten shares live in its room.state before it subscribes, as a browser would.
	for _, v := range viewers {
		for _, sh := range shares {
			v.waitLive(sh.id)
		}
	}
	wanted := func(j, i int) (protocol.SubscriptionWant, string) {
		if i == j {
			return want(shares[i].id, true), "f"
		}
		return want(shares[i].id, false), "q"
	}

	// ---- every viewer subscribes to every share, in one subscribe.update each ----
	start := time.Now()
	for j, v := range viewers {
		wants := make([]protocol.SubscriptionWant, n)
		for i := range n {
			wants[i], _ = wanted(j, i)
		}
		v.subscribe(wants...)
	}

	// ---- all hundred get their layer ----
	const frames = 10
	recs := make([][]*sfutest.Recorder, n)
	var slowest time.Duration
	for j := range n {
		recs[j] = make([]*sfutest.Recorder, n)
		for i := range n {
			_, rid := wanted(j, i)
			r := track(t, recorders[j], shares[i].id, webrtc.RTPCodecTypeVideo)
			waitFrames(t, r, 0, rid, frames)
			recs[j][i] = r
			slowest = max(slowest, r.Stats().First.Sub(start))
		}
	}
	// Let the streams run, then look at all of them.
	time.Sleep(time.Second)

	for j, v := range viewers {
		for i := range n {
			wnt, rid := wanted(j, i)
			focused := i == j
			what := fmt.Sprintf("viewer %d, share %d", j, i)
			vp := checkVideo(t, what+": video", recs[j][i].Packets())
			if runs := layerRuns(vp); runs != rid {
				t.Errorf("%s: the viewer got the layers %q, want only %s", what, runs, rid)
			}
			if st := recs[j][i].Stats(); st.Gaps != 0 || st.Duplicates != 0 {
				t.Errorf("%s: video stats on loopback = %+v", what, st)
			}
			// Audio only on the focused share.
			if got := hasTrack(recorders[j], shares[i].id, webrtc.RTPCodecTypeAudio); got != focused {
				t.Errorf("%s: the viewer has audio: %v, want %v", what, got, focused)
			}
			if focused {
				audio := track(t, recorders[j], shares[i].id, webrtc.RTPCodecTypeAudio)
				waitPackets(t, audio, 25)
				ap := audio.Packets()
				for _, check := range []func([]sfutest.Packet) error{sfutest.CheckContinuous, sfutest.CheckAudioMarkers} {
					if err := check(ap); err != nil {
						t.Errorf("%s: audio: %v", what, err)
					}
				}
			}
			// The viewer was told what it gets: all it asked for.
			v.waitStatus(wnt)
		}
		// One debounced sub offer carried the whole batch.
		if offers := v.subOffers(); len(offers) != 1 || len(offers[0].Tracks) != 2*n {
			t.Errorf("viewer %d: %d sub offers, want one that binds %d m-sections", j, len(offers), 2*n)
		}
		v.check()
	}

	// ---- what the room knows ----
	// Every share has all ten viewers as watchers: the one that has it in focus with the full layer and audio, the
	// other nine with the preview and none. The last sharer's room.state says so for every share.
	last := sharers[n-1]
	for i, sh := range shares {
		wantWatchers := make([]protocol.Watcher, n)
		for j, v := range viewers {
			wnt, _ := wanted(j, i)
			wantWatchers[j] = protocol.Watcher{UserID: v.userID, Video: wnt.Video, Audio: wnt.Audio}
		}
		byUser := func(a, b protocol.Watcher) int { return cmp.Compare(a.UserID, b.UserID) }
		slices.SortFunc(wantWatchers, byUser)
		var got []protocol.Watcher
		eventually(t, func() bool {
			info, ok := last.shareIn(sh.id)
			got = slices.Clone(info.Watchers)
			slices.SortFunc(got, byUser)
			return ok && info.Status == protocol.ShareStatusLive && slices.Equal(got, wantWatchers)
		}, func() string { return fmt.Sprintf("share %d has the watchers %+v, want %+v", i, got, wantWatchers) })
	}
	if st := last.roomState(); len(st.Participants) != 2*n || len(st.Shares) != n {
		t.Errorf("room.state has %d participants and %d shares, want %d and %d", len(st.Participants), len(st.Shares), 2*n, n)
	}
	for _, c := range sharers {
		c.check()
	}
	// Each layer had viewers arrive at once, and each got its keyframe: the publishers were asked, and not once per
	// viewer (the SFU's throttle is its own test's to measure).
	var requests int
	for i, sh := range shares {
		for _, layer := range []string{"f", "q"} {
			st := layerStats(t, sh.pub, layer)
			requests += st.PLIs + st.FIRs
			if st.PLIs+st.FIRs < 1 {
				t.Errorf("sharer %d, layer %s: no keyframe request, and viewers started on it", i, layer)
			}
		}
	}
	t.Logf("%d×%d: every subscription had its first packet %v after the viewers subscribed; %d keyframe requests for %d layers",
		n, n, slowest.Round(time.Millisecond), requests, 2*n)
}
