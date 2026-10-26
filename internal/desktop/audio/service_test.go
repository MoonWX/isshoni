package audio

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
)

// TestSubscribe: events reach every subscriber in order; a callback may call the service (its own events follow the
// current ones, without a deadlock); cancel removes a subscriber.
func TestSubscribe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctl := newStub()
		kook := app("kook.exe", "KOOK", verdictKeptOut, reasonMic)
		ctl.set(func(r *media.AudioReport) { r.Apps = []media.AudioApp{kook} })
		s := New(ctl, newStore(DefaultSettings()), testSelf, nil)
		var mu sync.Mutex
		var names []string
		var seqs []uint64
		cancel := s.Subscribe(func(ev Event) {
			mu.Lock()
			names = append(names, ev.Name)
			if fh, ok := ev.Payload.(FriendsHear); ok {
				seqs = append(seqs, fh.Seq)
			}
			mu.Unlock()
			if p, ok := ev.Payload.(Prompt); ok {
				_ = s.FriendsHear()
				if err := s.DismissPrompt(p.ID); err != nil { // a toast dismissed at once
					t.Errorf("DismissPrompt from a callback: %v", err)
				}
			}
		})
		other := record(s)
		stop := runService(t, s)
		defer stop()
		_ = s.Watch(true)
		settle(time.Second)
		mu.Lock()
		got := append([]string(nil), names...)
		gotSeqs := append([]uint64(nil), seqs...)
		mu.Unlock()
		if len(got) < 3 || got[len(got)-2] != EventPrompt || got[len(got)-1] != EventReport {
			t.Errorf("events %v", got)
		}
		for i := 1; i < len(gotSeqs); i++ {
			if gotSeqs[i] <= gotSeqs[i-1] {
				t.Errorf("seqs out of order: %v", gotSeqs)
			}
		}
		if n := len(other.take()); n != len(got) {
			t.Errorf("the other subscriber got %d events, the first %d", n, len(got))
		}
		if fh := s.FriendsHear(); len(fh.Prompts) != 0 {
			t.Errorf("prompts %+v", fh.Prompts)
		}
		cancel()
		s.OnShareStarted(0, true)
		mu.Lock()
		n := len(names)
		mu.Unlock()
		if n != len(got) {
			t.Errorf("a cancelled subscriber got %d more events", n-len(got))
		}
		s.OnShareStopped()
		stop()
	})
}

// TestRunOnceAndClose: Run returns when its ctx ends or on Close, and refuses a second call; WatchLevels, SetVisible
// and the getters work without Run.
func TestRunOnceAndClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New(newStub(), newStore(DefaultSettings()), testSelf, nil)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- s.Run(ctx) }()
		synctest.Wait()
		if err := s.Run(ctx); err == nil {
			t.Error("a second Run")
		}
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
		s.SetVisible(false)
		s.WatchLevels(true)
		if fh := s.FriendsHear(); fh.Seq != 1 || fh.Engine != "none" {
			t.Errorf("FriendsHear %+v", fh)
		}
		_ = s.Close()

		s2 := New(newStub(), newStore(DefaultSettings()), testSelf, nil)
		go func() { done <- s2.Run(context.Background()) }()
		synctest.Wait()
		_ = s2.Close()
		if err := <-done; err != nil {
			t.Errorf("Run after Close: %v", err)
		}
	})
}

// TestConcurrent exercises the service from many goroutines at once under the race detector.
func TestConcurrent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		eng := newFake("normal")
		defer func() { _ = eng.Close() }()
		s := New(eng, newStore(DefaultSettings()), testSelf, nil)
		s.Subscribe(func(Event) { _ = s.FriendsHear() })
		stop := runService(t, s)
		defer stop()
		var wg sync.WaitGroup
		for i := range 8 {
			wg.Go(func() {
				for j := range 20 {
					switch (i + j) % 8 {
					case 0:
						_ = s.Watch(j%2 == 0)
					case 1:
						s.WatchLevels(j%3 != 0)
					case 2:
						s.SetSharedPID(uint32(j))
					case 3:
						s.SetViewerBrowser("c", "chrome", "windows", j%2 == 0)
					case 4:
						_ = s.SetAppDecision("exe:kook.exe", []Decision{DecisionKeepOut, DecisionInclude, DecisionAuto}[j%3])
					case 5:
						_ = s.AudioConfig()
					case 6:
						_ = s.SetKeepOutMicUsers(j%2 == 0)
					case 7:
						_ = s.AppRules()
					}
					time.Sleep(30 * time.Millisecond)
				}
			})
		}
		wg.Wait()
		stop()
	})
}
