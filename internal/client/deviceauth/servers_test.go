package deviceauth_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"go.uber.org/goleak"

	"github.com/MoonWX/isshoni/internal/client/credstore"
	"github.com/MoonWX/isshoni/internal/client/deeplink"
	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// openServers opens Servers on dir with the fake server's network; closed by t.Cleanup.
func openServers(t *testing.T, fs *fakeServer, dir string, store credstore.Store) *deviceauth.Servers {
	t.Helper()
	s, err := deviceauth.OpenServers(deviceauth.ServersOptions{
		Dir:                   dir,
		Store:                 store,
		App:                   testApp,
		HTTP:                  fs.httpClient(),
		AllowInsecureLoopback: true,
		Logger:                quiet,
	})
	if err != nil {
		t.Fatalf("OpenServers: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// portOrigin is a loopback origin per port: the fake network reaches the one fake server for all of them.
func portOrigin(port int) string { return "http://127.0.0.1:" + strconv.Itoa(port) }

// claim makes the fake server's /info name origin as its public URL.
func claim(fs *fakeServer, origin, name string) {
	fs.with(func(fs *fakeServer) {
		fs.info.Server.PublicURL = origin
		fs.info.Server.Name = name
	})
}

func readServersFile(t *testing.T, dir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "servers.json"))
	must(t, err)
	var doc map[string]any
	must(t, json.Unmarshal(data, &doc))
	return doc
}

// servers.json round trip: what Add, a link and SetActive wrote comes back after a restart, in 02 §11.6's shape.
func TestServersRoundTrip(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fs := newFakeServer(t)
		dir := t.TempDir()
		store := credstore.NewMemory()
		s := openServers(t, fs, dir, store)
		if s.Active() != nil || len(s.List()) != 0 {
			t.Fatal("a new list isn't empty")
		}
		claim(fs, portOrigin(8443), "Alex's server")
		first, err := s.Add(t.Context(), portOrigin(8443))
		must(t, err)
		must(t, first.Manager.LinkWithPassword(t.Context(), "Alex", "correct horse"))
		claim(fs, portOrigin(9443), "Sam's server")
		second, err := s.Add(t.Context(), portOrigin(9443))
		must(t, err)
		if s.Active() != second.Manager {
			t.Fatal("the added server isn't active")
		}
		must(t, s.SetActive(portOrigin(8443)))
		want := s.List()
		must(t, s.Close())

		doc := readServersFile(t, dir)
		if doc["version"] != float64(1) || doc["active"] != portOrigin(8443) {
			t.Fatalf("servers.json = %v", doc)
		}
		entries := doc["servers"].([]any)
		e0 := entries[0].(map[string]any)
		for _, k := range []string{"origin", "name", "addedAt", "user", "device"} {
			if _, has := e0[k]; !has {
				t.Errorf("entry has no %q: %v", k, e0)
			}
		}
		if user := e0["user"].(map[string]any); user["id"] != testUserID || user["username"] != "Alex" || user["role"] != "admin" {
			t.Errorf("user = %v", user)
		}
		if _, has := entries[1].(map[string]any)["user"]; has {
			t.Error("the unlinked server has a user")
		}
		if runtime.GOOS != "windows" {
			info, err := os.Stat(filepath.Join(dir, "servers.json"))
			must(t, err)
			if mode := info.Mode().Perm(); mode != 0o600 {
				t.Errorf("servers.json mode %o, want 600", mode)
			}
		}

		again := openServers(t, fs, dir, store)
		got := again.List()
		if len(got) != 2 {
			t.Fatalf("List after reopening = %+v", got)
		}
		for i := range got {
			if got[i].Origin != want[i].Origin || got[i].Name != want[i].Name || !got[i].AddedAt.Equal(want[i].AddedAt) ||
				(got[i].User == nil) != (want[i].User == nil) || (got[i].Device == nil) != (want[i].Device == nil) {
				t.Fatalf("entry %d = %+v, want %+v", i, got[i], want[i])
			}
		}
		if *got[0].User != *want[0].User || *got[0].Device != *want[0].Device {
			t.Fatalf("user/device = %+v %+v", got[0].User, got[0].Device)
		}
		if a := again.Active(); a == nil || a.State().Origin != portOrigin(8443) {
			t.Fatal("the active server didn't survive the restart")
		}
		// Before Start, the state follows the store: linked with the stored credential, unlinked without one.
		if st := again.Active().State(); st.Status != deviceauth.StatusLinked || st.ServerName != "Alex's server" {
			t.Fatalf("the linked server's state before Start = %s %q, want linked", st.Status, st.ServerName)
		}
		must(t, again.SetActive(portOrigin(9443)))
		if st := again.Active().State(); st.Status != deviceauth.StatusUnlinked {
			t.Fatalf("the unlinked server's state before Start = %s", st.Status)
		}
		must(t, again.SetActive(portOrigin(8443)))
		if !again.Has(portOrigin(9443)) || !again.Has("HTTP://127.0.0.1:9443/") || again.Has(portOrigin(1)) {
			t.Fatal("Has")
		}
		// The reopened manager reuses the credential.
		tok, err := again.Active().AccessToken(t.Context(), false)
		must(t, err)
		if tok.Reveal() == "" {
			t.Fatal("no token")
		}
	})
}

// Add adopts the server's own public URL: entry and credential key; RequestedOrigin keeps what was typed. Adding
// an adopted origin again makes it active without a second entry.
func TestServersAddAdoptsThePublicURL(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fs := newFakeServer(t)
		store := credstore.NewMemory()
		s := openServers(t, fs, t.TempDir(), store)
		claim(fs, "http://LOCALHOST:8443/", "Alex's server")
		res, err := s.Add(t.Context(), "127.0.0.1:8443") // typed without a scheme: https…
		if err == nil {
			t.Fatalf("Add without http in a loopback dev setup = %+v; want the https request to fail", res)
		}
		res, err = s.Add(t.Context(), "http://127.0.0.1:8443")
		must(t, err)
		if res.Origin != "http://localhost:8443" || res.RequestedOrigin != "http://127.0.0.1:8443" || res.Existing {
			t.Fatalf("Add = %+v", res)
		}
		must(t, res.Manager.LinkWithPassword(t.Context(), "Alex", "correct horse"))
		if origins, _ := store.List(); !slices.Equal(origins, []string{"http://localhost:8443"}) {
			t.Fatalf("credential keys %v, want the adopted origin", origins)
		}
		for _, r := range fs.seen("/api/v1/device/password") {
			if r.Host != "localhost:8443" {
				t.Fatalf("the manager talks to %s, want the adopted origin", r.Host)
			}
		}
		again, err := s.Add(t.Context(), "http://[::1]:8443")
		must(t, err)
		if !again.Existing || again.Origin != "http://localhost:8443" || again.Manager != res.Manager || len(s.List()) != 1 {
			t.Fatalf("second Add = %+v, %d entries", again, len(s.List()))
		}
		claim(fs, "https://watch.example.com/path", "x")
		if _, err := s.Add(t.Context(), "http://127.0.0.1:9000"); !errors.Is(err, deviceauth.ErrBadResponse) {
			t.Fatalf("Add with a publicUrl that isn't an origin = %s", describe(err))
		}
		if _, err := s.Add(t.Context(), "ftp://x"); !errors.Is(err, deeplink.ErrInvalidOrigin) {
			t.Fatalf("Add(ftp://x) = %s", describe(err))
		}
		if err := s.SetActive(portOrigin(1)); err == nil {
			t.Fatal("SetActive of an unknown server succeeded")
		}
	})
}

// At most 20 servers: a 21st is ErrTooManyServers, while adding a listed one again still works.
func TestServersCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fs := newFakeServer(t)
		dir := t.TempDir()
		s := openServers(t, fs, dir, credstore.NewMemory())
		for i := range 20 {
			claim(fs, portOrigin(9000+i), fmt.Sprintf("server %d", i))
			if _, err := s.Add(t.Context(), portOrigin(9000+i)); err != nil {
				t.Fatalf("Add %d: %v", i, err)
			}
		}
		claim(fs, portOrigin(9020), "one too many")
		if _, err := s.Add(t.Context(), portOrigin(9020)); !errors.Is(err, deviceauth.ErrTooManyServers) {
			t.Fatalf("21st Add = %s, want ErrTooManyServers", describe(err))
		}
		claim(fs, portOrigin(9005), "server 5")
		res, err := s.Add(t.Context(), portOrigin(9005))
		if err != nil || !res.Existing {
			t.Fatalf("re-adding a listed server at the cap = %+v, %v", res, err)
		}
		if n := len(s.List()); n != 20 {
			t.Fatalf("%d servers", n)
		}
		must(t, s.Close())

		// A hand-edited file with more than 20 keeps the first 20.
		doc := readServersFile(t, dir)
		entries := doc["servers"].([]any)
		for i := 20; i < 25; i++ {
			entries = append(entries, map[string]any{"origin": portOrigin(9000 + i), "name": "extra", "addedAt": time.Now()})
		}
		doc["servers"] = entries
		data, err := json.Marshal(doc)
		must(t, err)
		must(t, os.WriteFile(filepath.Join(dir, "servers.json"), data, 0o600))
		if n := len(openServers(t, fs, dir, credstore.NewMemory()).List()); n != 20 {
			t.Fatalf("%d servers from a file with 25, want 20", n)
		}
	})
}

// A servers.json that doesn't parse, or has an unknown version, is renamed servers.json.bad and the list starts
// empty; credentials are untouched, so adding the server again reuses its credential.
func TestServersCorruptFile(t *testing.T) {
	for name, content := range map[string]string{
		"garbage":         "{not json",
		"unknown version": `{"version": 2, "active": "", "servers": []}`,
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fs := newFakeServer(t)
				dir := t.TempDir()
				path := filepath.Join(dir, "servers.json")
				must(t, os.WriteFile(path, []byte(content), 0o600))
				store := credstore.NewMemory()
				seedCredential(t, store, testOrigin)
				s := openServers(t, fs, dir, store)
				if len(s.List()) != 0 || s.Active() != nil {
					t.Fatal("the list isn't empty")
				}
				bad, err := os.ReadFile(path + ".bad")
				must(t, err)
				if string(bad) != content {
					t.Fatalf("servers.json.bad = %q", bad)
				}
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("servers.json still there: %v", err)
				}
				if got := storedToken(t, store, testOrigin); got != seedToken {
					t.Fatal("the credential was touched")
				}
				res, err := s.Add(t.Context(), testOrigin)
				must(t, err)
				if st := res.Manager.State(); st.Status != deviceauth.StatusLinked {
					t.Fatalf("state %s before Start with the kept credential, want linked", st.Status)
				}
				must(t, res.Manager.Start(t.Context()))
				wantStatus(t, res.Manager, deviceauth.StatusLinked, deviceauth.NoticeNone)
				if refs := fs.refreshes(); len(refs) != 1 || refs[0].RefreshToken != seedToken {
					t.Fatalf("refreshes %+v, want one with the kept credential", refs)
				}
				if doc := readServersFile(t, dir); doc["version"] != float64(1) {
					t.Fatalf("new servers.json = %v", doc)
				}
			})
		})
	}
}

// A refresh updates the entry's user and device; invalid_grant and Unlink clear them, and the server stays listed.
func TestServersEntryFollowsTheLink(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fs := newFakeServer(t)
		dir := t.TempDir()
		store := credstore.NewMemory()
		s := openServers(t, fs, dir, store)
		res, err := s.Add(t.Context(), testOrigin)
		must(t, err)
		must(t, res.Manager.LinkWithPassword(t.Context(), "Alex", "pw"))
		fs.with(func(fs *fakeServer) {
			fs.refresh = func(api.DeviceTokenRequest, int) reply {
				p := fs.pairLocked()
				p.Device.Name = "Renamed PC"
				p.User.Username = "Alexandra"
				return ok(p)
			}
		})
		_, err = res.Manager.AccessToken(t.Context(), true)
		must(t, err)
		e := s.List()[0]
		if e.Device == nil || e.Device.Name != "Renamed PC" || e.User == nil || e.User.Username != "Alexandra" {
			t.Fatalf("entry after the refresh = %+v %+v", e.User, e.Device)
		}
		fs.with(func(fs *fakeServer) {
			fs.refresh = func(api.DeviceTokenRequest, int) reply { return apiErr(api.CodeInvalidGrant, nil) }
		})
		time.Sleep(11 * time.Second)
		_, _ = res.Manager.AccessToken(t.Context(), true)
		e = s.List()[0]
		if e.User != nil || e.Device != nil {
			t.Fatalf("entry after invalid_grant = %+v %+v", e.User, e.Device)
		}
		doc := readServersFile(t, dir)
		if _, has := doc["servers"].([]any)[0].(map[string]any)["user"]; has {
			t.Fatal("servers.json kept the user")
		}
	})
}

// Remove unlinks, then drops the entry; the first remaining server becomes active.
func TestServersRemove(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fs := newFakeServer(t)
		store := credstore.NewMemory()
		s := openServers(t, fs, t.TempDir(), store)
		claim(fs, portOrigin(8443), "a")
		a, err := s.Add(t.Context(), portOrigin(8443))
		must(t, err)
		claim(fs, portOrigin(9443), "b")
		b, err := s.Add(t.Context(), portOrigin(9443))
		must(t, err)
		must(t, b.Manager.LinkWithPassword(t.Context(), "Alex", "pw"))
		must(t, s.Remove(t.Context(), portOrigin(9443)))
		if s.Has(portOrigin(9443)) || s.Active() != a.Manager || len(s.List()) != 1 {
			t.Fatalf("after Remove: %+v", s.List())
		}
		if got := storedToken(t, store, portOrigin(9443)); got != "" {
			t.Fatal("the removed server's credential is still stored")
		}
		if n := len(fs.seen("/api/v1/device/revoke")); n != 1 {
			t.Fatalf("%d revokes", n)
		}
		if err := s.Remove(t.Context(), portOrigin(9443)); err == nil {
			t.Fatal("removing an unknown server succeeded")
		}
		// Offline: the entry goes anyway, and the answer says the revoke wasn't confirmed.
		must(t, a.Manager.LinkWithPassword(t.Context(), "Alex", "pw"))
		fs.ln.down.Store(true)
		if err := s.Remove(t.Context(), portOrigin(8443)); !errors.Is(err, deviceauth.ErrRevokeNotConfirmed) {
			t.Fatalf("Remove offline = %s, want ErrRevokeNotConfirmed", describe(err))
		}
		if len(s.List()) != 0 || s.Active() != nil {
			t.Fatalf("after the last Remove: %+v", s.List())
		}
	})
}

// ForgetAll with 20 servers, half of them hanging, plus a credential servers.json doesn't name: it returns within
// 6 s, every credential is gone, users and devices are cleared, and each server that answers got its revoke.
func TestServersForgetAll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fs := newFakeServer(t)
		dir := t.TempDir()
		store := credstore.NewMemory()
		var entries []map[string]any
		for i := range 20 {
			origin := portOrigin(9000 + i)
			seedCredential(t, store, origin)
			entries = append(entries, map[string]any{
				"origin": origin, "name": "s", "addedAt": time.Now(),
				"user":   map[string]any{"id": testUserID, "username": "Alex", "role": "admin"},
				"device": map[string]any{"id": testDevice, "name": "Test PC"},
			})
		}
		seedCredential(t, store, portOrigin(9100)) // in the store only
		data, err := json.Marshal(map[string]any{"version": 1, "active": portOrigin(9000), "servers": entries})
		must(t, err)
		must(t, os.WriteFile(filepath.Join(dir, "servers.json"), data, 0o600))
		hanging := func(r *http.Request) bool {
			_, port, _ := net.SplitHostPort(r.Host)
			n, _ := strconv.Atoi(port)
			return n%2 == 1 && n < 9100
		}
		fs.with(func(fs *fakeServer) { fs.hang = hanging })
		s := openServers(t, fs, dir, store)
		start := time.Now()
		err = s.ForgetAll(t.Context())
		if d := time.Since(start); d > 6*time.Second {
			t.Fatalf("ForgetAll took %v, want at most 6 s", d)
		}
		must(t, err)
		if origins, err := store.List(); err != nil || len(origins) != 0 {
			t.Fatalf("credentials left: %v, %v", origins, err)
		}
		for _, e := range s.List() {
			if e.User != nil || e.Device != nil {
				t.Fatalf("entry %s keeps its user or device", e.Origin)
			}
		}
		if n := len(s.List()); n != 20 {
			t.Fatalf("%d servers listed after ForgetAll, want 20 (the list stays)", n)
		}
		for _, e := range readServersFile(t, dir)["servers"].([]any) {
			if _, has := e.(map[string]any)["user"]; has {
				t.Fatal("servers.json keeps a user")
			}
		}
		revoked := map[string]bool{}
		for _, r := range fs.seen("/api/v1/device/revoke") {
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer isa_") {
				t.Fatalf("revoke without a fresh access token: %q", r.Header.Get("Authorization"))
			}
			revoked[r.Host] = true
		}
		for i := range 20 {
			host := "127.0.0.1:" + strconv.Itoa(9000+i)
			if want := i%2 == 0; revoked[host] != want {
				t.Errorf("%s revoked = %v, want %v", host, revoked[host], want)
			}
		}
		if !revoked["127.0.0.1:9100"] {
			t.Error("the credential servers.json doesn't name wasn't revoked")
		}
		if a := s.Active(); a == nil || a.State().Status != deviceauth.StatusUnlinked {
			t.Fatal("the active manager isn't unlinked")
		}
	})
}

// A manager whose credential lives only in memory (save_failed) is revoked by ForgetAll too.
func TestServersForgetAllRunOnlyCredential(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fs := newFakeServer(t)
		log := &eventLog{}
		store := &orderStore{Store: credstore.NewMemory(), log: log}
		s := openServers(t, fs, t.TempDir(), store)
		res, err := s.Add(t.Context(), testOrigin)
		must(t, err)
		store.failAll.Store(true)
		must(t, res.Manager.LinkWithPassword(t.Context(), "Alex", "pw"))
		wantStatus(t, res.Manager, deviceauth.StatusLinked, deviceauth.NoticeSaveFailed)
		must(t, s.ForgetAll(t.Context()))
		revokes := fs.seen("/api/v1/device/revoke")
		if len(revokes) != 1 || revokes[0].Header.Get("Authorization") != "Bearer "+accessToken(1) {
			t.Fatalf("revokes %+v", revokes)
		}
		wantStatus(t, res.Manager, deviceauth.StatusUnlinked, deviceauth.NoticeNone)
	})
}

// With HTTP left nil, the package's own keep-alive Transport holds idle connections; Close closes them, so no
// connection goroutine outlives it (internal/server/itest's goleak check has no ignore for them). A real loopback
// server, outside any bubble: the in-memory network of the other tests never keeps a connection alive.
func TestCloseLeavesNoConnections(t *testing.T) {
	t.Run("Servers", func(t *testing.T) {
		fs := newTCPFakeServer(t)
		before := goleak.IgnoreCurrent()
		s, err := deviceauth.OpenServers(deviceauth.ServersOptions{
			Dir:                   t.TempDir(),
			Store:                 credstore.NewMemory(),
			App:                   testApp,
			AllowInsecureLoopback: true,
			Logger:                quiet,
		})
		must(t, err)
		res, err := s.Add(t.Context(), fs.origin)
		must(t, err)
		must(t, res.Manager.LinkWithPassword(t.Context(), "Alex", "correct horse"))
		must(t, s.Close())
		if err := goleak.Find(before); err != nil {
			t.Fatalf("goroutines outlive Close: %v", err)
		}
	})
	t.Run("Manager", func(t *testing.T) {
		fs := newTCPFakeServer(t)
		before := goleak.IgnoreCurrent()
		c, err := deviceauth.NewClient(fs.origin, testApp, deviceauth.ClientOptions{AllowInsecureLoopback: true})
		must(t, err)
		m := deviceauth.NewManager(deviceauth.ManagerOptions{Client: c, Store: credstore.NewMemory(), Logger: quiet})
		must(t, m.LinkWithPassword(t.Context(), "Alex", "correct horse"))
		_, err = m.AccessToken(t.Context(), true)
		must(t, err)
		must(t, m.Close())
		if err := goleak.Find(before); err != nil {
			t.Fatalf("goroutines outlive Close: %v", err)
		}
	})
}

func TestOpenServersNeedsDirAndStore(t *testing.T) {
	if _, err := deviceauth.OpenServers(deviceauth.ServersOptions{Store: credstore.NewMemory()}); err == nil {
		t.Error("OpenServers without Dir succeeded")
	}
	if _, err := deviceauth.OpenServers(deviceauth.ServersOptions{Dir: t.TempDir()}); err == nil {
		t.Error("OpenServers without Store succeeded")
	}
}
