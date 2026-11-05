package app

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/client/core"
	"github.com/MoonWX/isshoni/internal/desktop/audio"
	"github.com/MoonWX/isshoni/internal/desktop/i18n"
	"github.com/MoonWX/isshoni/internal/desktop/session"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/web"
)

// recAudio is 05's service with the calls the app makes recorded: SetVisible, SetViewerBrowser, Decide and
// DismissPrompt; emit hands an event to Subscribe's callback, as 05 does.
type recAudio struct {
	mu        sync.Mutex
	calls     []string
	fn        func(audio.Event)
	decideErr error
}

func (r *recAudio) add(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, fmt.Sprintf(format, args...))
}

func (r *recAudio) get() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

func (r *recAudio) Run(ctx context.Context) error { <-ctx.Done(); return nil }
func (r *recAudio) Close() error                  { return nil }
func (r *recAudio) SetVisible(v bool)             { r.add("visible(%v)", v) }
func (r *recAudio) SetViewerBrowser(conn, family, os string, playing bool) {
	r.add("viewer(%s,%s,%s,%v)", conn, family, os, playing)
}

func (r *recAudio) Decide(id string, d audio.Decision) error {
	r.add("decide(%s,%s)", id, d)
	return r.decideErr
}

func (r *recAudio) DismissPrompt(id string) error {
	r.add("dismiss(%s)", id)
	return nil
}

func (r *recAudio) Subscribe(fn func(audio.Event)) func() {
	r.mu.Lock()
	r.fn = fn
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		r.fn = nil
		r.mu.Unlock()
	}
}

func (r *recAudio) emit(ev audio.Event) {
	r.mu.Lock()
	fn := r.fn
	r.mu.Unlock()
	if fn != nil {
		fn(ev)
	}
}

// eventClient is a C2 core client whose events the test sends.
type eventClient struct {
	mu     sync.Mutex
	ch     chan core.Event
	conn   string
	unsubs int
}

func newEventClient() *eventClient {
	return &eventClient{ch: make(chan core.Event, 8), conn: core.ConnOnline}
}

func (c *eventClient) Run(ctx context.Context) error { <-ctx.Done(); return nil }
func (c *eventClient) Status() core.Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return core.Status{Conn: c.conn, ConnectionID: "c_pub"}
}
func (c *eventClient) Subscribe() (<-chan core.Event, func()) {
	return c.ch, func() { c.mu.Lock(); c.unsubs++; c.mu.Unlock() }
}
func (c *eventClient) StartShare(context.Context, core.ShareRequest) (*core.Share, error) {
	return nil, core.ErrNotReady
}
func (c *eventClient) StopOther(context.Context, string, string) error { return nil }
func (c *eventClient) WatchServerStats(bool)                           {}
func (c *eventClient) Close() error                                    { return nil }

// eventPub is C2 with one online client.
type eventPub struct {
	fakePub
	c *eventClient
}

func (p *eventPub) Ready(context.Context) (session.Client, string, error) {
	return p.c, "https://a.example.com", nil
}

// newAudioApp is an app over recAudio, sysShell, the desktop catalog and pub (nil: no C2), running.
func newAudioApp(t *testing.T, pub publisher) (*App, *recAudio, *sysShell) {
	t.Helper()
	return newAudioAppWith(t, pub, func(s *sysShell) Shell { return s })
}

// newAudioAppWith is newAudioApp with the shell the app gets wrapped by wrap.
func newAudioAppWith(t *testing.T, pub publisher, wrap func(*sysShell) Shell) (*App, *recAudio, *sysShell) {
	t.Helper()
	e := newEnv(t, nil, nil)
	cat, err := i18n.Load(web.DesktopCatalog(), nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := &recAudio{}
	shell := &sysShell{fakeShell: e.shell}
	d := Deps{Settings: e.app.d.Settings, Servers: e.srv, Audio: rec, Shell: wrap(shell), OS: fakeOS{}, I18n: cat,
		Log: e.app.d.Log}
	if pub != nil {
		d.Session = pub.(Session)
	}
	a, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = a.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return a, rec, shell
}

// waitFor polls cond for up to 3 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); !cond(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// TestPlaybackStateToViewerBrowser is 03 §12.2's receiver: C2's EventPlaybackState reaches 05's SetViewerBrowser as
// it is, and a sender that leaves C1's user.connections (App.OwnConnections) is reported as stopped, once.
func TestPlaybackStateToViewerBrowser(t *testing.T) {
	c := newEventClient()
	a, rec, _ := newAudioApp(t, &eventPub{c: c})
	if err := a.OwnConnections([]string{"c_tab1", "c_tab2", "c_phone"}); err != nil {
		t.Fatal(err)
	}
	c.ch <- core.Event{Kind: core.EventPlaybackState, Playback: &core.PlaybackState{From: "c_tab1", Playing: true,
		Browser: "firefox", OS: protocol.ClientOSWindows}}
	c.ch <- core.Event{Kind: core.EventPlaybackState, Playback: &core.PlaybackState{From: "c_tab2", Playing: true,
		Browser: "chrome", OS: protocol.ClientOSWindows}}
	c.ch <- core.Event{Kind: core.EventPlaybackState, Playback: &core.PlaybackState{From: "c_tab2", Playing: false,
		Browser: "chrome", OS: protocol.ClientOSWindows}}
	want := []string{"viewer(c_tab1,firefox,windows,true)", "viewer(c_tab2,chrome,windows,true)",
		"viewer(c_tab2,chrome,windows,false)"}
	waitFor(t, "the three reports", func() bool { return len(viewerCalls(rec)) == 3 })
	if got := viewerCalls(rec); !slices.Equal(got, want) {
		t.Fatalf("calls %q, want %q", got, want)
	}
	// c_tab2 already stopped and c_phone never played: only c_tab1 is expired when all three leave.
	if err := a.OwnConnections([]string{"c_other"}); err != nil {
		t.Fatal(err)
	}
	if err := a.OwnConnections(nil); err != nil {
		t.Fatal(err)
	}
	want = append(want, "viewer(c_tab1,,,false)")
	if got := viewerCalls(rec); !slices.Equal(got, want) {
		t.Fatalf("calls %q, want %q", got, want)
	}
}

func viewerCalls(rec *recAudio) []string {
	var out []string
	for _, c := range rec.get() {
		if len(c) > 7 && c[:7] == "viewer(" {
			out = append(out, c)
		}
	}
	return out
}

// TestPlaybackFollowsNextClient: a core client that stops is left, and the next one C2 builds is followed.
func TestPlaybackFollowsNextClient(t *testing.T) {
	first := newEventClient()
	pub := &swapPub{c: first}
	_, rec, _ := newAudioApp(t, pub)
	first.ch <- core.Event{Kind: core.EventPlaybackState, Playback: &core.PlaybackState{From: "c_a", Playing: true,
		Browser: "edge", OS: protocol.ClientOSWindows}}
	waitFor(t, "the first client's report", func() bool { return len(viewerCalls(rec)) == 1 })
	second := newEventClient()
	pub.set(second)
	first.mu.Lock()
	first.conn = core.ConnStopped
	first.mu.Unlock()
	second.ch <- core.Event{Kind: core.EventPlaybackState, Playback: &core.PlaybackState{From: "c_b", Playing: true,
		Browser: "chrome", OS: protocol.ClientOSWindows}}
	waitFor(t, "the second client's report", func() bool { return len(viewerCalls(rec)) == 2 })
	if got := viewerCalls(rec)[1]; got != "viewer(c_b,chrome,windows,true)" {
		t.Errorf("second report %q", got)
	}
}

// swapPub is C2 whose online client the test replaces.
type swapPub struct {
	fakePub
	mu sync.Mutex
	c  *eventClient
}

func (p *swapPub) set(c *eventClient) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.c = c
}

func (p *swapPub) Ready(context.Context) (session.Client, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.c, "https://a.example.com", nil
}

// TestMicPromptToast is 03 §15's mic prompt rows: a prompt while the window is hidden becomes a toast with the
// prompt's texts by kind; Keep out and Include call 05's Decide once; a visible window gets no toast; a report that
// no longer lists the prompt settles its toast; a dismissed toast dismisses the prompt.
func TestMicPromptToast(t *testing.T) {
	a, rec, shell := newAudioApp(t, nil)
	waitFor(t, "Run's SetVisible", func() bool { return len(rec.get()) > 0 })
	if got := rec.get(); got[0] != "visible(true)" {
		t.Fatalf("SetVisible at start: %q", got)
	}
	waitFor(t, "the subscription to 05's events", func() bool {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return rec.fn != nil
	})
	toasts := func() []Notification {
		shell.mu.Lock()
		defer shell.mu.Unlock()
		return slices.Clone(shell.toasts)
	}

	rec.emit(audio.Event{Name: audio.EventPrompt, Payload: audio.Prompt{ID: "p1", AppID: "exe:kook.exe",
		Name: "KOOK", Kind: audio.PromptMic}})
	if n := len(toasts()); n != 0 {
		t.Fatalf("%d toasts while the window is visible", n)
	}

	a.HideWindow()
	rec.emit(audio.Event{Name: audio.EventPrompt, Payload: audio.Prompt{ID: "p2", AppID: "exe:kook.exe",
		Name: "KOOK", Kind: audio.PromptMic}})
	rec.emit(audio.Event{Name: audio.EventPrompt, Payload: audio.Prompt{ID: "p3", AppID: "exe:chrome.exe",
		Name: "Google Chrome", Kind: audio.PromptBrowserMic}})
	got := toasts()
	if len(got) != 2 {
		t.Fatalf("toasts %+v", got)
	}
	mic, browser := got[0], got[1]
	if mic.Kind != ToastMicPrompt || mic.Body != "KOOK is using your microphone. Keep it out of your stream?" ||
		mic.Nonce == "" || len(mic.Actions) != 2 || mic.Actions[0].ID != ToastKeepOut ||
		mic.Actions[0].Title != "Keep out" || mic.Actions[1].ID != ToastInclude || mic.Actions[1].Title != "Include" {
		t.Errorf("mic toast %+v", mic)
	}
	if browser.Actions[0].Title != "Keep browser out" || browser.Body == mic.Body || browser.Body == "" {
		t.Errorf("browser toast %+v", browser)
	}

	if !a.ToastAction(mic.Nonce, ToastKeepOut) {
		t.Fatal("Keep out wasn't taken")
	}
	if a.ToastAction(mic.Nonce, ToastInclude) {
		t.Error("a settled toast acted again")
	}
	// The browser prompt is answered in the window: the next report doesn't list it, so its toast is settled.
	rec.emit(audio.Event{Name: audio.EventReport, Payload: audio.FriendsHear{}})
	if a.ToastAction(browser.Nonce, ToastInclude) {
		t.Error("the toast of a prompt answered in the window acted")
	}
	if n := countPrefix(rec.get(), "decide("); n != 1 || !slices.Contains(rec.get(), "decide(p2,keep-out)") {
		t.Errorf("calls %q", rec.get())
	}

	// A dismissed toast dismisses its prompt (05's DismissPrompt), once.
	rec.emit(audio.Event{Name: audio.EventPrompt, Payload: audio.Prompt{ID: "p4", AppID: "exe:kook.exe",
		Name: "KOOK", Kind: audio.PromptMic}})
	p4 := toasts()[2]
	a.ToastDismissed(p4.Nonce)
	a.ToastDismissed(p4.Nonce)
	if a.ToastAction(p4.Nonce, ToastInclude) {
		t.Error("a dismissed toast acted")
	}
	if n := countPrefix(rec.get(), "dismiss(p4)"); n != 1 {
		t.Errorf("calls %q", rec.get())
	}

	// Include on a prompt that was answered meanwhile is no error the user sees.
	rec.emit(audio.Event{Name: audio.EventPrompt, Payload: audio.Prompt{ID: "p5", AppID: "exe:kook.exe",
		Name: "KOOK", Kind: audio.PromptMic}})
	rec.mu.Lock()
	rec.decideErr = audio.ErrUnknownPrompt
	rec.mu.Unlock()
	if a.ToastAction(toasts()[3].Nonce, ToastInclude) {
		t.Error("an answer to a prompt that is gone ran")
	}
}

// focusShell is a shell that reports whether its window has the focus (the app's focusReporter), as the Wails shell
// does.
type focusShell struct {
	*sysShell
	focused atomic.Bool
}

func (s *focusShell) WindowFocused() bool { return s.WindowVisible() && s.focused.Load() }

// TestMicPromptToastUnfocused is 03 §15's "whenever the window isn't in the foreground": a window that is visible
// but not focused (minimised, or behind a full-screen game) gets the prompt as a toast with Keep out and Include; a
// focused one gets none.
func TestMicPromptToastUnfocused(t *testing.T) {
	var fs *focusShell
	a, rec, shell := newAudioAppWith(t, nil, func(s *sysShell) Shell {
		fs = &focusShell{sysShell: s}
		fs.focused.Store(true)
		return fs
	})
	waitFor(t, "the subscription to 05's events", func() bool {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return rec.fn != nil
	})
	toasts := func() []Notification {
		shell.mu.Lock()
		defer shell.mu.Unlock()
		return slices.Clone(shell.toasts)
	}
	if !shell.WindowVisible() {
		t.Fatal("the window isn't visible")
	}

	rec.emit(audio.Event{Name: audio.EventPrompt, Payload: audio.Prompt{ID: "p1", AppID: "exe:kook.exe",
		Name: "KOOK", Kind: audio.PromptMic}})
	if n := len(toasts()); n != 0 {
		t.Fatalf("%d toasts while the window is in front", n)
	}

	fs.focused.Store(false)
	rec.emit(audio.Event{Name: audio.EventPrompt, Payload: audio.Prompt{ID: "p2", AppID: "exe:kook.exe",
		Name: "KOOK", Kind: audio.PromptMic}})
	got := toasts()
	if len(got) != 1 {
		t.Fatalf("toasts %+v", got)
	}
	mic := got[0]
	if mic.Kind != ToastMicPrompt || mic.Body != "KOOK is using your microphone. Keep it out of your stream?" ||
		len(mic.Actions) != 2 || mic.Actions[0].ID != ToastKeepOut || mic.Actions[0].Title != "Keep out" ||
		mic.Actions[1].ID != ToastInclude || mic.Actions[1].Title != "Include" {
		t.Errorf("mic toast %+v", mic)
	}
	if !a.ToastAction(mic.Nonce, ToastInclude) || !slices.Contains(rec.get(), "decide(p2,include)") {
		t.Errorf("calls %q", rec.get())
	}
}

func countPrefix(calls []string, prefix string) int {
	n := 0
	for _, c := range calls {
		if len(c) >= len(prefix) && c[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}
