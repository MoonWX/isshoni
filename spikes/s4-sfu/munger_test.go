package main

import (
	"testing"
	"time"
)

type testPkt struct {
	rid      string
	seq      uint16
	ts       uint32
	keyframe bool
}

// feed runs packets through the munger and returns the forwarded ones.
func feed(m *munger, start time.Time, pkts []testPkt) (out []testPkt) {
	for i, p := range pkts {
		seq, ts, ok := m.process(p.rid, p.seq, p.ts, p.keyframe, start.Add(time.Duration(i)*10*time.Millisecond))
		if ok {
			out = append(out, testPkt{p.rid, seq, ts, p.keyframe})
		}
	}
	return out
}

func assertContinuous(t *testing.T, out []testPkt) {
	t.Helper()
	for i := 1; i < len(out); i++ {
		if out[i].seq != out[i-1].seq+1 {
			t.Fatalf("seq gap at %d: %d -> %d", i, out[i-1].seq, out[i].seq)
		}
		if int32(out[i].ts-out[i-1].ts) < 0 {
			t.Fatalf("timestamp went backwards at %d: %d -> %d", i, out[i-1].ts, out[i].ts)
		}
	}
}

func TestMungerSwitchesOnlyOnKeyframe(t *testing.T) {
	m := &munger{clockRate: 90000}
	m.setTarget("f", true)
	now := time.Now()

	// Layer f starts mid-GOP: nothing forwarded until its keyframe.
	out := feed(m, now, []testPkt{
		{"f", 100, 1000, false}, {"q", 500, 7000, true},
		{"f", 101, 4000, true}, {"f", 102, 4000, false}, {"q", 501, 13000, false},
		{"f", 103, 7000, false},
	})
	if len(out) != 3 || out[0].rid != "f" || !out[0].keyframe {
		t.Fatalf("want 3 packets starting at f keyframe, got %+v", out)
	}

	// Switch to q: keep forwarding f until q has a keyframe, then only q.
	m.setTarget("q", true)
	out2 := feed(m, now.Add(time.Second), []testPkt{
		{"q", 502, 16000, false}, {"f", 104, 10000, false},
		{"q", 503, 19000, true}, {"f", 105, 13000, false}, {"q", 504, 22000, false},
	})
	if len(out2) != 3 || out2[0].rid != "f" || out2[1].rid != "q" || !out2[1].keyframe || out2[2].rid != "q" {
		t.Fatalf("unexpected switch sequence %+v", out2)
	}
	assertContinuous(t, append(out, out2...))
	if m.switches != 2 {
		t.Fatalf("want 2 switches, got %d", m.switches)
	}
}

func TestMungerPauseResumeKeepsContinuity(t *testing.T) {
	m := &munger{clockRate: 90000}
	m.setTarget("f", true)
	now := time.Now()
	out := feed(m, now, []testPkt{{"f", 10, 100, true}, {"f", 11, 3100, false}})

	m.setTarget("", false)
	if got := feed(m, now, []testPkt{{"f", 12, 6100, true}}); len(got) != 0 {
		t.Fatalf("paused munger forwarded %+v", got)
	}

	m.setTarget("f", true)
	out2 := feed(m, now.Add(2*time.Second), []testPkt{
		{"f", 13, 9100, false}, // not a keyframe: must wait
		{"f", 14, 12100, true}, {"f", 15, 15100, false},
	})
	if len(out2) != 2 || !out2[0].keyframe {
		t.Fatalf("resume should start at a keyframe, got %+v", out2)
	}
	assertContinuous(t, append(out, out2...))
}

func TestMungerDropsReorderedPacketsFromBeforeSwitch(t *testing.T) {
	m := &munger{clockRate: 90000}
	m.setTarget("f", true)
	now := time.Now()
	out := feed(m, now, []testPkt{{"f", 10, 100, true}, {"f", 11, 100, false}})
	m.setTarget("q", true)
	out2 := feed(m, now, []testPkt{
		{"q", 200, 5000, true},
		{"q", 199, 2000, false}, // arrived late, predates the switch point
		{"q", 201, 5000, false},
	})
	if len(out2) != 2 {
		t.Fatalf("late pre-switch packet must be dropped, got %+v", out2)
	}
	assertContinuous(t, append(out, out2...))
}

func TestMungerAudioAlwaysForwardsWhenActive(t *testing.T) {
	m := &munger{clockRate: 48000}
	m.setTarget("", true)
	out := feed(m, time.Now(), []testPkt{{"", 1, 960, true}, {"", 2, 1920, true}, {"", 3, 2880, true}})
	if len(out) != 3 {
		t.Fatalf("want 3 audio packets, got %d", len(out))
	}
	assertContinuous(t, out)
}

func TestIsH264KeyframeStart(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
		want    bool
	}{
		{"sps", []byte{0x67, 0x42}, true},
		{"idr alone", []byte{0x65, 0x88}, false},
		{"non-idr", []byte{0x41, 0x9a}, false},
		{"stap-a sps+pps", []byte{0x78, 0x00, 0x02, 0x67, 0x42, 0x00, 0x02, 0x68, 0xce}, true},
		{"stap-a without sps", []byte{0x78, 0x00, 0x02, 0x06, 0x05}, false},
		{"stap-a truncated", []byte{0x78, 0x00, 0x09, 0x67}, false},
		{"fu-a idr start", []byte{0x7c, 0x85}, false},
		{"empty", nil, false},
	}
	for _, c := range cases {
		if got := isH264KeyframeStart(c.payload); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestMatchCodecPrefersSameProfile(t *testing.T) {
	want := h264Cap("640c1f")
	have := []testCodec{{96, "42e01f"}, {102, "640c1f"}}
	got, match := matchCodec(want, toParams(have))
	if match != "exact" || got.PayloadType != 102 {
		t.Fatalf("want exact PT 102, got %s PT %d", match, got.PayloadType)
	}
	got, match = matchCodec(want, toParams(have[:1]))
	if match != "partial" || got.PayloadType != 96 {
		t.Fatalf("want partial PT 96, got %s PT %d", match, got.PayloadType)
	}
}
