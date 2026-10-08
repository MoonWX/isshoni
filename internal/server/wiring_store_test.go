package server_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/server"
	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/servertest"
)

// dbFiles lists the database's files in the data directory with their sizes, and checks that each is private.
func dbFiles(t *testing.T, srv *servertest.Server) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	entries, err := os.ReadDir(srv.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "isshoni.db") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %v, want 0600", e.Name(), fi.Mode().Perm())
		}
		out[e.Name()] = fi.Size()
	}
	return out
}

// TestStoreLifecycle: the database is in the data directory, private, from the first start (03 §4.1); a shutdown
// closes it last, with the WAL checkpointed, so the .db file alone is the whole database (04 §6.4 step 6).
func TestStoreLifecycle(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{Deps: wiredDeps()})
	setUp(t, srv)
	if files := dbFiles(t, srv); files["isshoni.db"] == 0 {
		t.Fatalf("database files while the server runs = %v", files)
	}
	if fi, err := os.Stat(srv.Cfg.Paths().Backups); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Errorf("the backups directory: %v, %v", fi, err)
	}
	srv.Stop(t)
	if files := dbFiles(t, srv); files["isshoni.db"] == 0 || files["isshoni.db-wal"] != 0 {
		t.Errorf("database files after the shutdown = %v, want the .db file alone", files)
	}

	t.Run("a shutdown that runs out of time", func(t *testing.T) {
		// A request that never ends uses up the whole shutdown_timeout. The store is closed all the same, and
		// completely: what was written just before is in the .db file.
		started := make(chan struct{})
		deps := wiredDeps()
		deps.API = http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			close(started)
			<-r.Context().Done()
		})
		hung := servertest.Start(t, servertest.Options{Deps: deps, Config: func(c *config.Config) {
			c.ShutdownTimeout = config.Duration{Duration: 300 * time.Millisecond}
		}})
		admin, ctx := adminSocket(t, hung)
		if _, err := admin.SetupURL(ctx, 0); err != nil { // a write: the setup token and its audit row
			t.Fatal(err)
		}
		go func() { _, _ = tryDo(hung, http.MethodGet, "/api/v1/hang") }()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("the request did not reach its handler")
		}

		begin := time.Now()
		err := hung.Srv.Shutdown(context.Background(), server.ShutdownStop)
		if !errors.Is(err, server.ErrShutdownForced) || strings.Contains(err.Error(), "store") {
			t.Errorf("Shutdown = %v, want a forced shutdown in which the store still closed", err)
		}
		if took := time.Since(begin); took > 2*time.Second {
			t.Errorf("Shutdown took %v with shutdown_timeout = 300ms", took)
		}
		if files := dbFiles(t, hung); files["isshoni.db"] == 0 || files["isshoni.db-wal"] != 0 {
			t.Errorf("database files after the forced shutdown = %v, want the .db file alone", files)
		}
		if err := hung.Wait(t); !errors.Is(err, server.ErrShutdownForced) {
			t.Errorf("Run = %v, want the forced shutdown's error", err)
		}
	})
}
