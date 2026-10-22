package deviceauth

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/MoonWX/isshoni/internal/client/credstore"
	"github.com/MoonWX/isshoni/internal/client/deeplink"
	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// servers.json (02 §11.6).
const (
	serversFile    = "servers.json"
	serversVersion = 1
	maxServers     = 20
	forgetTimeout  = 5 * time.Second // ForgetAll's one budget for every server's refresh and revoke
)

// ServersOptions configures OpenServers.
type ServersOptions struct {
	Dir                   string // the data root, %LOCALAPPDATA%\isshoni on Windows (03-desktop-app §14.1 passes it)
	Store                 credstore.Store
	App                   AppInfo
	HTTP                  *http.Client // nil = one DefaultHTTPClient() shared by every server, closed with Servers
	AllowInsecureLoopback bool
	OnState               func(State) // per server; State.Origin says which
	Clock                 func() time.Time
	Logger                *slog.Logger
}

// ServerEntry is one server of servers.json.
type ServerEntry struct {
	Origin  string         `json:"origin"` // the server's own public origin (Add), also the credential key
	Name    string         `json:"name"`   // server name from /info
	AddedAt time.Time      `json:"addedAt"`
	User    *api.User      `json:"user,omitempty"`
	Device  *api.DeviceRef `json:"device,omitempty"`
}

// AddResult is what Add did. Origin is the entry's origin; RequestedOrigin is what the user typed or the link
// named, normalized. When their hosts differ, 03-desktop-app shows "This server calls itself {host}".
type AddResult struct {
	Manager         *Manager
	Origin          string
	RequestedOrigin string
	Existing        bool // Origin was already in the list: that entry became active, nothing was added
}

// Servers is the app's list of servers (02 §11.6): servers.json in the data root, one Manager per server, one
// active server. Safe for concurrent use.
//
// servers.json is written atomically (a temporary file in the same folder, then a rename), mode 0600 on Unix (on
// Windows it inherits the user profile's ACL), and holds at most 20 servers. A file that doesn't parse or has an
// unknown version is renamed to servers.json.bad and the list starts empty (a WARN); credentials are not touched,
// so adding a server again reuses its credential.
//
// OpenServers creates the managers and starts none: the app calls Start on the active one at boot and after Add or
// SetActive. Each manager reads its credential when it is made, so its State() is linked or unlinked before Start.
// Close closes them all.
type Servers struct {
	o       ServersOptions
	path    string
	log     *slog.Logger
	ownHTTP bool // o.HTTP is OpenServers' own DefaultHTTPClient: Close closes its idle connections

	mu      sync.Mutex
	closed  bool
	entries []ServerEntry
	mgrs    map[string]*Manager
	active  string
}

// serversDoc is servers.json.
type serversDoc struct {
	Version int           `json:"version"`
	Active  string        `json:"active"`
	Servers []ServerEntry `json:"servers"`
}

// OpenServers reads servers.json from o.Dir (none yet is an empty list) and creates a Manager for every server.
// o.Dir and o.Store are required.
func OpenServers(o ServersOptions) (*Servers, error) {
	if o.Dir == "" || o.Store == nil {
		return nil, errors.New("deviceauth: OpenServers needs a Dir and a Store")
	}
	ownHTTP := o.HTTP == nil
	if ownHTTP {
		o.HTTP = DefaultHTTPClient()
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	s := &Servers{
		o:       o,
		path:    filepath.Join(o.Dir, serversFile),
		log:     o.Logger.With("component", "deviceauth"),
		ownHTTP: ownHTTP,
		mgrs:    make(map[string]*Manager),
	}
	doc, err := s.load()
	if err != nil {
		return nil, err
	}
	skipped := 0
	for _, e := range doc.Servers {
		norm, err := deeplink.NormalizeOrigin(e.Origin, o.AllowInsecureLoopback)
		if err != nil || norm != e.Origin || s.mgrs[e.Origin] != nil || len(s.entries) == maxServers {
			skipped++
			continue
		}
		m, err := s.newManager(e.Origin, e.Name)
		if err != nil {
			skipped++
			continue
		}
		_ = m.load() // best effort, so State() follows the store before Start; Start reports a store error
		s.entries = append(s.entries, cloneEntry(e))
		s.mgrs[e.Origin] = m
	}
	if skipped > 0 {
		s.log.Warn("deviceauth: servers.json lists servers this build can't use; left them out", "count", skipped)
	}
	if s.mgrs[doc.Active] != nil {
		s.active = doc.Active
	} else if len(s.entries) > 0 {
		s.active = s.entries[0].Origin
	}
	return s, nil
}

// load reads servers.json. A missing file is an empty list; one that doesn't parse or has another version is set
// aside as servers.json.bad (02 §11.6).
func (s *Servers) load() (serversDoc, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return serversDoc{Version: serversVersion}, nil
	}
	if err != nil {
		return serversDoc{}, fmt.Errorf("deviceauth: read servers.json: %w", err)
	}
	var doc serversDoc
	if err := json.Unmarshal(data, &doc); err != nil || doc.Version != serversVersion {
		if err := os.Rename(s.path, s.path+".bad"); err != nil {
			s.log.Warn("deviceauth: couldn't set the unreadable servers.json aside", "err", err)
		}
		s.log.Warn("deviceauth: servers.json can't be read; renamed it to servers.json.bad and started with no servers")
		return serversDoc{Version: serversVersion}, nil
	}
	return doc, nil
}

// newManager returns a manager for an origin of the list.
func (s *Servers) newManager(origin, name string) (*Manager, error) {
	c, err := NewClient(origin, s.o.App, ClientOptions{HTTP: s.o.HTTP, AllowInsecureLoopback: s.o.AllowInsecureLoopback})
	if err != nil {
		return nil, err
	}
	m := NewManager(ManagerOptions{Client: c, Store: s.o.Store, OnState: s.o.OnState, Clock: s.o.Clock, Logger: s.o.Logger})
	m.state.ServerName = name
	m.lastQueued = m.state.clone()
	m.entry = func(ch entryChange) { s.applyEntry(origin, ch) }
	return m, nil
}

// List returns the servers in the order they were added.
func (s *Servers) List() []ServerEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ServerEntry, len(s.entries))
	for i, e := range s.entries {
		out[i] = cloneEntry(e)
	}
	return out
}

// Has reports whether origin (any spelling NormalizeOrigin turns into a listed origin) is in the list.
func (s *Servers) Has(origin string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mgrs[s.canon(origin)] != nil
}

// Active returns the active server's manager; nil when no server was added.
func (s *Servers) Active() *Manager {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mgrs[s.active]
}

// Add runs after the user confirmed (02 §12.3); the entry becomes active. It normalizes origin
// (deeplink.NormalizeOrigin; an error wraps deeplink.ErrInvalidOrigin), asks its /info, and stores
// NormalizeOrigin(info.server.publicUrl) as the entry's origin and credential key, not the origin as typed: the
// server's Host, Origin and CSRF checks use its public origin. A publicUrl that doesn't normalize is ErrBadResponse.
// When the adopted origin is already listed, that entry becomes active and Existing is true; otherwise a 21st
// server is ErrTooManyServers.
func (s *Servers) Add(ctx context.Context, origin string) (AddResult, error) {
	requested, err := deeplink.NormalizeOrigin(origin, s.o.AllowInsecureLoopback)
	if err != nil {
		return AddResult{}, fmt.Errorf("deviceauth: %w", err)
	}
	if s.isClosed() {
		return AddResult{}, errClosed
	}
	c, err := NewClient(requested, s.o.App, ClientOptions{HTTP: s.o.HTTP, AllowInsecureLoopback: s.o.AllowInsecureLoopback})
	if err != nil {
		return AddResult{}, err
	}
	info, err := c.Info(ctx)
	if err != nil {
		return AddResult{}, err
	}
	adopted, err := deeplink.NormalizeOrigin(info.Server.PublicURL, s.o.AllowInsecureLoopback)
	if err != nil {
		return AddResult{}, badResponse(0, "the server's publicUrl isn't an origin")
	}
	res, err := s.addEntry(requested, adopted, info.Server.Name)
	if err == nil && !res.Existing {
		// A credential still stored for the server (servers.json was set aside, 02 §11.6) is linked before Start.
		// Best effort, outside s.mu: Start reports a store error.
		_ = res.Manager.load()
	}
	return res, err
}

// addEntry is Add's change of the list, under s.mu.
func (s *Servers) addEntry(requested, adopted, name string) (AddResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return AddResult{}, errClosed
	}
	if m := s.mgrs[adopted]; m != nil {
		prev := s.active
		s.active = adopted
		if i := s.indexLocked(adopted); name != "" && s.entries[i].Name != name {
			s.entries[i].Name = name
		}
		if err := s.saveLocked(); err != nil {
			s.active = prev
			return AddResult{}, err
		}
		return AddResult{Manager: m, Origin: adopted, RequestedOrigin: requested, Existing: true}, nil
	}
	if len(s.entries) >= maxServers {
		return AddResult{}, ErrTooManyServers
	}
	m, err := s.newManager(adopted, name)
	if err != nil {
		return AddResult{}, err
	}
	prev := s.active
	s.entries = append(s.entries, ServerEntry{
		Origin:  adopted,
		Name:    name,
		AddedAt: s.o.Clock().UTC().Truncate(time.Millisecond),
	})
	s.mgrs[adopted] = m
	s.active = adopted
	if err := s.saveLocked(); err != nil {
		s.entries = s.entries[:len(s.entries)-1]
		delete(s.mgrs, adopted)
		s.active = prev
		_ = m.Close()
		return AddResult{}, err
	}
	return AddResult{Manager: m, Origin: adopted, RequestedOrigin: requested}, nil
}

// SetActive makes a listed server the active one.
func (s *Servers) SetActive(origin string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errClosed
	}
	o := s.canon(origin)
	if s.mgrs[o] == nil {
		return errUnknownServer
	}
	prev := s.active
	s.active = o
	if err := s.saveLocked(); err != nil {
		s.active = prev
		return err
	}
	return nil
}

// Remove signs the app out of a server (Manager.Unlink), then drops its entry; when it was the active one, the first
// remaining server becomes active. The entry is dropped in every case: ErrRevokeNotConfirmed says the server
// wasn't reached, so the device may still be on the user's Devices page.
func (s *Servers) Remove(ctx context.Context, origin string) error {
	s.mu.Lock()
	o := s.canon(origin)
	m := s.mgrs[o]
	closed := s.closed
	s.mu.Unlock()
	switch {
	case closed:
		return errClosed
	case m == nil:
		return errUnknownServer
	}
	unlinkErr := m.Unlink(ctx)
	s.mu.Lock()
	var saveErr error
	if i := s.indexLocked(o); i >= 0 && s.mgrs[o] == m {
		s.entries = slices.Delete(s.entries, i, i+1)
		delete(s.mgrs, o)
		if s.active == o {
			s.active = ""
			if len(s.entries) > 0 {
				s.active = s.entries[0].Origin
			}
		}
		saveErr = s.saveLocked()
	}
	s.mu.Unlock()
	_ = m.Close()
	if saveErr != nil {
		return saveErr
	}
	return unlinkErr
}

// ForgetAll is what the uninstaller's --forget-credentials and --purge-data run (02 §11.7, 06's "DeleteAll"):
//  1. every credential under the store's prefix (credstore List), whether or not servers.json names it, and every
//     listed server's credential (one only in memory after save_failed included);
//  2. for all of them concurrently, under one shared 5 s deadline (and ctx): refresh, then POST /device/revoke
//     with the new access token; a failure of either is ignored;
//  3. every credential is deleted whatever the revoke results, every manager is unlinked, and the user and
//     device of every entry in servers.json are cleared.
//
// The call takes about 5 s at most whatever the number of servers. It returns the errors of the local steps (the
// store's List and Delete, the servers.json write), joined; the uninstaller exits 0 anyway.
func (s *Servers) ForgetAll(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, forgetTimeout)
	defer cancel()
	s.mu.Lock()
	mgrs := maps.Clone(s.mgrs)
	s.mu.Unlock()
	listed, listErr := s.o.Store.List()
	origins := make(map[string]bool, len(listed)+len(mgrs))
	for _, o := range listed {
		origins[o] = true
	}
	for o := range mgrs {
		origins[o] = true
	}
	var wg sync.WaitGroup
	for o := range origins {
		wg.Go(func() { s.revokeOne(ctx, o, mgrs[o]) })
	}
	wg.Wait()
	var errs []error
	if listErr != nil {
		errs = append(errs, fmt.Errorf("deviceauth: list the credentials: %w", listErr))
	}
	for _, o := range slices.Sorted(maps.Keys(origins)) {
		if err := s.o.Store.Delete(o); err != nil {
			errs = append(errs, fmt.Errorf("deviceauth: delete a credential: %w", err))
		}
	}
	s.mu.Lock()
	for i := range s.entries {
		s.entries[i].User, s.entries[i].Device = nil, nil
	}
	if err := s.saveLocked(); err != nil {
		errs = append(errs, err)
	}
	s.mu.Unlock()
	s.log.Info("deviceauth: forgot every credential", "count", len(origins))
	return errors.Join(errs...)
}

// revokeOne is ForgetAll's step 2 for one origin: with its manager when it has one (the manager forgets its
// credential in memory first, so nothing refreshes meanwhile), else with the stored credential.
func (s *Servers) revokeOne(ctx context.Context, origin string, m *Manager) {
	if m != nil {
		_ = m.load()
		if t, ok := m.takeCredential(); ok {
			if t.cred != nil {
				revokeDevice(ctx, m.c, m.rand, *t.cred, t.access, t.exp, m.now())
			}
			return
		}
	}
	cred, err := s.o.Store.Get(origin)
	if err != nil {
		return
	}
	c, err := NewClient(origin, s.o.App, ClientOptions{HTTP: s.o.HTTP, AllowInsecureLoopback: s.o.AllowInsecureLoopback})
	if err != nil {
		return
	}
	revokeDevice(ctx, c, rand.Reader, cred, "", time.Time{}, s.o.Clock())
}

// Close closes every manager, and the idle connections of the HTTP client OpenServers made itself (HTTP nil), so
// no connection goroutine outlives it. Servers can't be used afterwards; Close is idempotent.
func (s *Servers) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	mgrs := slices.Collect(maps.Values(s.mgrs))
	s.mu.Unlock()
	for _, m := range mgrs {
		_ = m.Close()
	}
	if s.ownHTTP {
		s.o.HTTP.CloseIdleConnections()
	}
	return nil
}

func (s *Servers) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// canon returns origin normalized, or as given when it doesn't normalize (so it matches nothing).
func (s *Servers) canon(origin string) string {
	if o, err := deeplink.NormalizeOrigin(origin, s.o.AllowInsecureLoopback); err == nil {
		return o
	}
	return origin
}

func (s *Servers) indexLocked(origin string) int {
	return slices.IndexFunc(s.entries, func(e ServerEntry) bool { return e.Origin == origin })
}

// applyEntry is a manager's entryChange: the name from /info, the user and device after a link or a refresh, or
// cleared after invalid_grant and Unlink. servers.json is written only when something changed.
func (s *Servers) applyEntry(origin string, ch entryChange) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.indexLocked(origin)
	if i < 0 {
		return
	}
	e := &s.entries[i]
	changed := false
	if ch.name != "" && ch.name != e.Name {
		e.Name = ch.name
		changed = true
	}
	if ch.setLink && (!sameUser(e.User, ch.user) || !sameDevice(e.Device, ch.device)) {
		e.User, e.Device = cloneUser(ch.user), cloneDevice(ch.device)
		changed = true
	}
	if !changed {
		return
	}
	if err := s.saveLocked(); err != nil {
		s.log.Warn("deviceauth: couldn't write servers.json", "err", err)
	}
}

// saveLocked writes servers.json.
func (s *Servers) saveLocked() error {
	doc := serversDoc{Version: serversVersion, Active: s.active, Servers: s.entries}
	if doc.Servers == nil {
		doc.Servers = []ServerEntry{}
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("deviceauth: encode servers.json: %w", err)
	}
	if err := writeFileAtomic(s.path, append(data, '\n')); err != nil {
		return fmt.Errorf("deviceauth: write servers.json: %w", err)
	}
	return nil
}

// writeFileAtomic replaces path with data: a temporary file in the same folder (mode 0600), written and synced,
// then renamed over the old one. On any failure the old file stays as it was and the temporary file is removed.
func writeFileAtomic(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func cloneEntry(e ServerEntry) ServerEntry {
	e.User, e.Device = cloneUser(e.User), cloneDevice(e.Device)
	return e
}

func cloneUser(u *api.User) *api.User {
	if u == nil {
		return nil
	}
	c := *u
	return &c
}

func cloneDevice(d *api.DeviceRef) *api.DeviceRef {
	if d == nil {
		return nil
	}
	c := *d
	return &c
}
