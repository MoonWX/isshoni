package sfu

import (
	"slices"
	"testing"
	"time"
)

// policy_test of 02 §17 (README S69): the room codec policy of 02 §8.3 with a fake clock. High → cb at once; cb →
// high only after 60 s without a viewer that needs it; Conns without H.264 (and Conns that don't subscribe) are no
// viewers; a change is reported to the hub and, per share, to the Conn that publishes it.

// TestCodecPolicyNext drives the policy's hysteresis by hand: the times are the fake clock.
func TestCodecPolicyNext(t *testing.T) {
	const s = int64(time.Second)
	hold := 60 * time.Second
	type step struct {
		want    ProfileKey
		at      int64
		changed bool
		profile ProfileKey
	}
	for _, tc := range []struct {
		name  string
		hold  time.Duration
		steps []step
	}{
		{"high stays high", hold, []step{
			{ProfileHigh, 0, false, ProfileHigh},
			{ProfileHigh, 100 * s, false, ProfileHigh},
		}},
		{"high to cb at once, back after 60 s", hold, []step{
			{ProfileConstrainedBaseline, 5 * s, true, ProfileConstrainedBaseline},
			{ProfileConstrainedBaseline, 30 * s, false, ProfileConstrainedBaseline},
			{ProfileHigh, 40 * s, false, ProfileConstrainedBaseline}, // the hold starts here
			{ProfileHigh, 99*s + s - 1, false, ProfileConstrainedBaseline},
			{ProfileHigh, 100 * s, true, ProfileHigh},
			{ProfileHigh, 200 * s, false, ProfileHigh},
		}},
		{"a viewer who comes back starts the hold again", hold, []step{
			{ProfileConstrainedBaseline, 0, true, ProfileConstrainedBaseline},
			{ProfileHigh, 10 * s, false, ProfileConstrainedBaseline},
			{ProfileConstrainedBaseline, 50 * s, false, ProfileConstrainedBaseline},
			{ProfileHigh, 60 * s, false, ProfileConstrainedBaseline},
			{ProfileHigh, 110 * s, false, ProfileConstrainedBaseline}, // 60 s after 10 s, but 50 s after 60 s
			{ProfileHigh, 120 * s, true, ProfileHigh},
		}},
		{"high to cb again at once after the way back", hold, []step{
			{ProfileConstrainedBaseline, 0, true, ProfileConstrainedBaseline},
			{ProfileHigh, 1 * s, false, ProfileConstrainedBaseline},
			{ProfileHigh, 61 * s, true, ProfileHigh},
			{ProfileConstrainedBaseline, 61 * s, true, ProfileConstrainedBaseline},
		}},
		{"a clock that runs back counts no time", hold, []step{
			{ProfileConstrainedBaseline, 100 * s, true, ProfileConstrainedBaseline},
			{ProfileHigh, 100 * s, false, ProfileConstrainedBaseline},
			{ProfileHigh, 10 * s, false, ProfileConstrainedBaseline},
			{ProfileHigh, 160 * s, true, ProfileHigh},
		}},
		{"the first moment of the clock still starts a hold", hold, []step{
			{ProfileConstrainedBaseline, 0, true, ProfileConstrainedBaseline},
			{ProfileHigh, 0, false, ProfileConstrainedBaseline},
			{ProfileHigh, 60 * s, true, ProfileHigh},
		}},
		{"no hold", 0, []step{
			{ProfileConstrainedBaseline, 0, true, ProfileConstrainedBaseline},
			{ProfileHigh, 0, true, ProfileHigh},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := codecPolicy{profile: ProfileHigh}
			for i, st := range tc.steps {
				if changed := p.next(st.want, st.at, tc.hold); changed != st.changed || p.profile != st.profile {
					t.Fatalf("step %d (%s at %v): changed %v, policy %s; want %v, %s", i, st.want,
						time.Duration(st.at), changed, p.profile, st.changed, st.profile)
				}
			}
		})
	}
}

// joinCaps joins a Conn with the given decode caps; the participant id is the user id.
func joinCaps(t *testing.T, s *SFU, user UserID, conn ConnID, role Role, caps ...ProfileKey) (*Conn, *recSignaler) {
	t.Helper()
	sig := &recSignaler{}
	c, err := s.Join(JoinParams{
		Room: "lounge", Participant: ParticipantID(user), User: user, Conn: conn, Role: role, Client: ClientWeb,
		Decode: DecodeCaps{H264: caps}, Signaler: sig,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, sig
}

// codecHints returns the CodecPolicyEvents a Conn's client got, in order, as "share:profile".
func (r *recSignaler) codecHints() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, ev := range r.events {
		if h, ok := ev.(CodecPolicyEvent); ok {
			out = append(out, string(h.Share)+":"+string(h.Profile))
		}
	}
	return out
}

func (e *recEvents) policies() []ProfileKey {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.policy)
}

// TestRoomCodecPolicy runs the policy through an SFU: the viewer set, the reports of a change, ShareParams, the
// hold on the fake clock, SetDecodeCaps and Resync.
func TestRoomCodecPolicy(t *testing.T) {
	s, ev := newTestSFU(t)
	ctx := testCtx(t)
	waitHints := func(sig *recSignaler, want ...string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !slices.Equal(sig.codecHints(), want) {
			if time.Now().After(deadline) {
				t.Fatalf("codec hints %v, want %v", sig.codecHints(), want)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	check := func(what string, wantPolicy ProfileKey, wantChanges ...ProfileKey) {
		t.Helper()
		if got := s.CodecPolicy("lounge"); got != wantPolicy {
			t.Errorf("%s: policy %s, want %s", what, got, wantPolicy)
		}
		if got := ev.policies(); !slices.Equal(got, wantChanges) {
			t.Errorf("%s: CodecPolicyChanged %v, want %v", what, got, wantChanges)
		}
	}

	pub, pubSig := joinCaps(t, s, "alice", "c-alice", RoleFull, ProfileHigh, ProfileConstrainedBaseline)
	if p := startShare(t, pub, "s_a"); p.Profile != ProfileHigh {
		t.Errorf("a share in a room of Chrome viewers gets profile %s", p.Profile)
	}
	check("Chrome only", ProfileHigh)

	// Conns without H.264 are waiting for a decoder, and Conns that don't subscribe watch nothing: neither counts.
	joinCaps(t, s, "dan", "c-fresh-firefox", RoleViewer)
	joinCaps(t, s, "erin", "c-desktop-core", RolePublisher, ProfileConstrainedBaseline)
	check("a fresh Firefox and a publisher", ProfileHigh)
	if s.CodecPolicy("nowhere") != ProfileHigh {
		t.Error("an unknown room is not high")
	}

	// A viewer that decodes only Constrained Baseline: cb at once, one hint per share to its publisher.
	ff, _ := joinCaps(t, s, "bob", "c-firefox", RoleViewer, ProfileConstrainedBaseline, ProfileBaseline)
	check("a Firefox viewer", ProfileConstrainedBaseline, ProfileConstrainedBaseline)
	waitHints(pubSig, "s_a:42e0")
	if p := startShare(t, pub, "s_b"); p.Profile != ProfileConstrainedBaseline {
		t.Errorf("a share started while Firefox watches gets profile %s", p.Profile)
	}

	// It leaves: the policy holds for 60 s on the codec clock, then goes back to high. (The 10 s short of the hold
	// leave room for the real time a loaded machine takes between the steps; TestCodecPolicyNext has the edges.)
	ff.Close(EndReasonLeft)
	waitDone(t, ff)
	s.CheckCodecPolicies()
	check("Firefox left", ProfileConstrainedBaseline, ProfileConstrainedBaseline)
	s.AdvanceCodecClock(codecPolicyHold - 10*time.Second)
	s.CheckCodecPolicies()
	check("50 s later", ProfileConstrainedBaseline, ProfileConstrainedBaseline)
	s.AdvanceCodecClock(10 * time.Second)
	s.CheckCodecPolicies()
	check("60 s later", ProfileHigh, ProfileConstrainedBaseline, ProfileHigh)
	waitHints(pubSig, "s_a:42e0", "s_a:6400", "s_b:6400")

	// The ticker does the same without being asked: a viewer whose caps lose High makes it cb, one whose caps gain
	// it again starts the hold, and the ticker ends it once the clock has moved on.
	c, _ := joinCaps(t, s, "carol", "c-carol", RoleFull, ProfileHigh)
	if err := c.SetDecodeCaps(ctx, DecodeCaps{H264: []ProfileKey{ProfileConstrainedBaseline}}); err != nil {
		t.Fatal(err)
	}
	check("caps without High", ProfileConstrainedBaseline, ProfileConstrainedBaseline, ProfileHigh, ProfileConstrainedBaseline)
	if err := c.SetDecodeCaps(ctx, DecodeCaps{H264: []ProfileKey{ProfileConstrainedHigh}}); err != nil {
		t.Fatal(err)
	}
	check("caps with Constrained High", ProfileConstrainedBaseline, ProfileConstrainedBaseline, ProfileHigh, ProfileConstrainedBaseline)
	s.AdvanceCodecClock(codecPolicyHold)
	deadline := time.Now().Add(5 * time.Second)
	for s.CodecPolicy("lounge") != ProfileHigh {
		if time.Now().After(deadline) {
			t.Fatal("the ticker did not end the hold")
		}
		time.Sleep(10 * time.Millisecond)
	}
	check("the ticker", ProfileHigh, ProfileConstrainedBaseline, ProfileHigh, ProfileConstrainedBaseline, ProfileHigh)
	waitHints(pubSig, "s_a:42e0", "s_a:6400", "s_b:6400", "s_a:42e0", "s_b:42e0", "s_a:6400", "s_b:6400")

	// A share that ended gets no more hints; Resync sends one per live share with the policy as it is.
	if err := pub.StopShare(ctx, "s_a", EndReasonStopped); err != nil {
		t.Fatal(err)
	}
	pub.Resync()
	waitHints(pubSig, "s_a:42e0", "s_a:6400", "s_b:6400", "s_a:42e0", "s_b:42e0", "s_a:6400", "s_b:6400", "s_b:6400")
}
