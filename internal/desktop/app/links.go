package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/MoonWX/isshoni/internal/client/deeplink"
	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/desktop/servers"
)

// MaxServers is 02's cap on the server list (03 §8.1, §20).
const MaxServers = 20

// linkInterval is the rate of 03 §13.2: at most one link handled per second.
const linkInterval = time.Second

// PendingLink is the deeplink.open payload and App.PendingLink's answer (03 §6.3): the link not yet acted on.
// Exactly one of its shapes is set:
//   - {roomId}: the active server's room ("" = the SPA's own last room, else welcome.defaultRoomId);
//   - {add}: an unknown server, "Add server {host}?" before any network request (03 §8.2, 02 §12.3);
//   - {switch}: a known server that isn't active while a share is live: "You're sharing in {room}. Open
//     {target}? Your share will stop." (02 §12.3);
//   - {error: "too_many_servers"}: an unknown server while 20 are listed ("You've added 20 servers. Remove one
//     first.").
type PendingLink struct {
	RoomID string     `json:"roomId"`
	Add    *LinkAdd   `json:"add,omitempty"`
	Switch *LinkAdd   `json:"switch,omitempty"`
	Error  string     `json:"error,omitempty"`
	Live   *LiveShare `json:"live,omitempty"` // with Switch: the share that would stop
}

// LinkAdd names the server of an add or switch question (deeplink.Link's fields).
type LinkAdd struct {
	Origin      string `json:"origin"`
	Host        string `json:"host"`
	HostUnicode string `json:"hostUnicode"`
	RoomID      string `json:"roomId"`
}

// LiveShare is the live share a switch question names.
type LiveShare struct {
	RoomID string `json:"roomId"`
}

// MarshalJSON writes only the set shape: {"roomId": …}, or {"add": …}, {"switch": …, "live": …} or {"error": …}.
func (p PendingLink) MarshalJSON() ([]byte, error) {
	if p.Add == nil && p.Switch == nil && p.Error == "" {
		return json.Marshal(struct {
			RoomID string `json:"roomId"`
		}{p.RoomID})
	}
	return json.Marshal(struct {
		Add    *LinkAdd   `json:"add,omitempty"`
		Switch *LinkAdd   `json:"switch,omitempty"`
		Live   *LiveShare `json:"live,omitempty"`
		Error  string     `json:"error,omitempty"`
	}{p.Add, p.Switch, p.Live, p.Error})
}

// confirmation reports whether p asks the user something: a link arriving meanwhile is ignored (03 §13.2: a newer
// link must not swap the host under the friend's cursor).
func (p *PendingLink) confirmation() bool { return p != nil && (p.Add != nil || p.Switch != nil) }

// Pending are the registry values HKCU\Software\isshoni\PendingServer and PendingRoom (03 §13.2, §14.1; 06 §2.3):
// written by 06's installer for /SERVER=, by this app before a start-time update, or by a start from a link that
// found an installer running.
type Pending struct {
	Server string // a server origin
	Room   string // a room id; only with Server
}

// PendingStore reads and deletes the Pending values (winos on Windows; nothing elsewhere).
type PendingStore interface {
	// TakePending reads both values and deletes them, also when they are invalid.
	TakePending() (Pending, error)
}

// TakePending is 03 §13.4 step 6: it reads and deletes the Pending values; main calls it before anything else of
// the app runs and hands the result to Boot. A nil store has none. An error is logged by the caller; the values
// read (if any) still count.
func TakePending(store PendingStore) (Pending, error) {
	if store == nil {
		return Pending{}, nil
	}
	return store.TakePending()
}

// Link turns the values into the deeplink.Link that deeplink.Parse returns for the same link (03 §13.2): the origin
// through deeplink.NormalizeOrigin(Server, insecureLoopback), the room by the room-id rule. An invalid room is
// dropped (the server is kept, with errInvalidRoom); an invalid or missing server gives an error and no link.
func (p Pending) Link(insecureLoopback bool) (deeplink.Link, error) {
	if p.Server == "" {
		return deeplink.Link{}, errNoPending
	}
	origin, err := deeplink.NormalizeOrigin(p.Server, insecureLoopback)
	if err != nil {
		return deeplink.Link{}, err
	}
	l, err := linkFor(origin, "")
	if err != nil {
		return deeplink.Link{}, err
	}
	if p.Room != "" {
		if !deeplink.ValidRoomID(p.Room) {
			return l, errInvalidRoom
		}
		l.RoomID = p.Room
	}
	return l, nil
}

var (
	errNoPending   = errors.New("app: no pending server")
	errInvalidRoom = errors.New("app: the pending room is not a room id; dropped")
)

// linkFor is the deeplink.Link of a normalized origin: Parse(Build(origin, room)) for https, and for a dev build's
// http loopback origin (which no link can carry) the same fields by hand.
func linkFor(origin, room string) (deeplink.Link, error) {
	if hostPort, ok := strings.CutPrefix(origin, "http://"); ok {
		return deeplink.Link{Origin: origin, Host: hostPort, HostUnicode: hostPort, RoomID: room}, nil
	}
	return deeplink.Parse(deeplink.Build(origin, room))
}

// Boot is the end of 03 §13.4 step 7 in the first instance, once the shell runs: HandleArgs(args), then HandleLink
// for the Pending values that TakePending read at step 6. An invalid value is dropped with a log line.
func (a *App) Boot(args []string, p Pending) {
	a.HandleArgs(args)
	if p.Server == "" && p.Room == "" {
		return
	}
	l, err := p.Link(a.d.InsecureLoopback)
	switch {
	case errors.Is(err, errInvalidRoom):
		a.log.Warn("app: the pending room is not a room id; opening the server only")
	case err != nil:
		a.log.Warn("app: the pending server is not a server address; dropped", "err", err)
		return
	}
	a.HandleLink(l)
}

// HandleArgs acts on a command line: the first instance's own (through Boot) and every one a second instance
// forwards (03 §13.1, §13.3). Only --open-url, --server and --autostart do anything here; one-shot modes are never
// honored (main ran them before the app existed, and a forwarded one is as untrusted as any argument). Ignored
// arguments are logged by name only.
func (a *App) HandleArgs(args []string) {
	p := ParseArgs(args)
	if len(p.Ignored) > 0 || p.OneShot != "" {
		ignored := p.Ignored
		if p.OneShot != "" {
			ignored = append(ignored, string(p.OneShot))
		}
		a.log.Info("app: arguments ignored", "args", ignored)
	}
	switch {
	case p.Link != "":
		l, err := deeplink.Parse(p.Link)
		if err != nil {
			// The rule that failed, the scheme and the host; never the path (03 §13.1).
			a.log.Info("app: a link that can't be used", "err", err, "scheme", "isshoni", "host", linkHost(p.Link))
			a.d.Shell.ShowWindow(true)
			return
		}
		a.HandleLink(l)
	case p.OpenURL:
		a.d.Shell.ShowWindow(true)
	case p.Server != "":
		addr, err := servers.Normalize(p.Server, a.d.InsecureLoopback)
		if err != nil {
			a.log.Info("app: --server is not a server address", "err", err)
			a.d.Shell.ShowWindow(true)
			return
		}
		l, err := linkFor(addr.Origin, "")
		if err != nil {
			a.log.Info("app: --server is not a server address", "err", err)
			a.d.Shell.ShowWindow(true)
			return
		}
		a.HandleLink(l)
	case p.Autostart:
		// A start at sign-in opens no window (03 §16.1); a forwarded one changes nothing.
	default:
		a.d.Shell.ShowWindow(true)
	}
}

// linkHost returns the host of a link for a log line, or "" when it can't be cut out cleanly: no userinfo, at most
// 253 characters of a host name, an IP literal and a port.
func linkHost(link string) string {
	_, rest, ok := strings.Cut(link, "://")
	if !ok {
		return ""
	}
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	if strings.Contains(rest, "@") || rest == "" || len(rest) > 260 {
		return ""
	}
	for _, c := range rest {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', strings.ContainsRune(".-:[]", c):
		default:
			return ""
		}
	}
	return strings.ToLower(rest)
}

// HandleLink handles a parsed link, --server or the Pending values (03 §13.2):
//
//   - a link while a confirmation is open or a code is shown (the link step): ignored;
//   - more than one link within a second: the extra ones are dropped;
//   - an unknown server: the window, and deeplink.open {add} ("Add server {host}?") before any network request;
//     with 20 servers listed, {error: too_many_servers} instead;
//   - the active server: the window, and deeplink.open {roomId};
//   - another known server while a share is live: the window, and deeplink.open {switch}, never a silent stop;
//   - another known server otherwise: it becomes active and the window reloads (which shows its link step when it
//     isn't linked); the reloaded page reads the room from PendingLink. Nothing is emitted to the page that is
//     about to go, so it can't act on another server's room.
func (a *App) HandleLink(l deeplink.Link) {
	a.mu.Lock()
	if a.pending.confirmation() {
		a.mu.Unlock()
		a.log.Debug("app: a link arrived while a question is open; ignored")
		return
	}
	a.mu.Unlock()
	srv := a.d.Servers
	active := srv.Active()
	if active != nil && active.State().Status == deviceauth.StatusLinking {
		a.log.Debug("app: a link arrived while the link step shows a code; ignored")
		return
	}
	now := a.clock()
	a.mu.Lock()
	if !a.lastLink.IsZero() && now.Sub(a.lastLink) < linkInterval && now.Sub(a.lastLink) >= 0 {
		a.mu.Unlock()
		a.log.Debug("app: more than one link in a second; dropped")
		return
	}
	a.lastLink = now
	a.mu.Unlock()

	list := srv.List()
	var entry *deviceauth.ServerEntry
	for i := range list {
		if list[i].Origin == l.Origin {
			entry = &list[i]
		}
	}
	activeOrigin := ""
	if active != nil {
		activeOrigin = active.State().Origin
	}
	switch {
	case entry == nil && len(list) >= MaxServers:
		a.log.Info("app: a link to a new server, but 20 are listed")
		a.offer(&PendingLink{Error: "too_many_servers"})
	case entry == nil:
		a.log.Info("app: a link to a server that isn't listed; asking first", "host", l.Host)
		a.offer(&PendingLink{Add: &LinkAdd{Origin: l.Origin, Host: l.Host, HostUnicode: l.HostUnicode, RoomID: l.RoomID}})
	case l.Origin == activeOrigin:
		a.offer(&PendingLink{RoomID: l.RoomID})
	default:
		if room, live := a.sharing(); live {
			a.offer(&PendingLink{
				Switch: &LinkAdd{Origin: l.Origin, Host: l.Host, HostUnicode: l.HostUnicode, RoomID: l.RoomID},
				Live:   &LiveShare{RoomID: room},
			})
			return
		}
		if err := a.switchServer(l.Origin, &PendingLink{RoomID: l.RoomID}); err != nil {
			a.log.Warn("app: the link's server couldn't be made active", "err", err)
		}
	}
}

// offer keeps p as the pending link, shows and focuses the window and emits deeplink.open.
func (a *App) offer(p *PendingLink) {
	a.mu.Lock()
	a.pending = p
	a.mu.Unlock()
	a.d.Shell.ShowWindow(true)
	a.d.Shell.Emit(EventDeepLinkOpen, *p)
}

// switchServer makes origin the active server, keeps next as the pending link for the reloaded page, reloads the
// window once and starts the new active server's manager (03 §8.1: every change of the active server reloads).
func (a *App) switchServer(origin string, next *PendingLink) error {
	if err := a.d.Servers.SetActive(origin); err != nil {
		return fmt.Errorf("app: %w", err)
	}
	a.mu.Lock()
	a.pending = next
	a.mu.Unlock()
	a.d.Shell.ShowWindow(true)
	a.d.Shell.Reload()
	a.startActive()
	return nil
}

// PendingLink returns the link not yet acted on, e.g. the one that started the app before the SPA ran
// (03 §6.2 App.PendingLink); nil when there is none.
func (a *App) PendingLink() *PendingLink {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pending == nil {
		return nil
	}
	p := *a.pending
	return &p
}

// LinkHandled clears the pending link: the SPA navigated (a room) or the question was answered (03 §6.2).
func (a *App) LinkHandled() {
	a.mu.Lock()
	a.pending = nil
	a.mu.Unlock()
}
