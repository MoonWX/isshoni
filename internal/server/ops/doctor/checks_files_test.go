package doctor

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/config"
)

// The config check passes on what the loader found: nothing, warnings, or errors (the first one with its fix).
func TestCheckConfig(t *testing.T) {
	w := newWorld(t, "--tls.mode", "ip")
	c := w.check("config")
	want(t, c, api.DoctorStatusOK, codeConfigOK, "")

	// A file that was read shows as its path.
	file := filepath.Join(t.TempDir(), "isshoni.toml")
	if err := os.WriteFile(file, []byte("domain = \"watch.example.com\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(flag.NewFlagSet("test", flag.ContinueOnError), []string{"--config", file}, []string{})
	if err != nil {
		t.Fatal(err)
	}
	w.env.Config = cfg
	c = w.check("config")
	want(t, c, api.DoctorStatusOK, codeConfigOK, "")
	if c.Message != file {
		t.Errorf("message %q, want the file's path", c.Message)
	}

	// Warnings: the first one, and how many there are.
	w = newWorld(t, "--tls.mode", "off", "--listen.http", "0.0.0.0:8080", "--public-url", "https://watch.example.com",
		"--listen.https", ":8443")
	c = w.check("config")
	want(t, c, api.DoctorStatusWarn, codeConfigWarnings, fixConfigCheck)
	contains(t, c.Message, "2 warnings: ", "listen.https")
	contains(t, c.Fix, "remove it", "`isshoni config check` lists every problem")

	// Errors: the server would exit 78 on this config.
	w = newWorld(t, "--tls.mode", "manual")
	c = w.check("config")
	want(t, c, api.DoctorStatusFail, codeConfigInvalid, fixConfigCheck)
	contains(t, c.Message, "1 error: tls.mode", "needs tls.cert_file and tls.key_file")
	contains(t, c.Fix, "set tls.cert_file to the certificate chain")
	if strings.Contains(c.Fix, "config check") {
		t.Errorf("one problem needs no pointer to config check: %q", c.Fix)
	}
	// In a container the command is the container's.
	w.container("docker", true)
	w.env.Config = loadConfig(t, "--data-dir", w.dataDir, "--tls.mode", "manual", "--listen.http", "nope")
	contains(t, w.check("config").Fix, "`docker compose exec isshoni isshoni config check` lists every problem")
}

func TestCheckDataDir(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		w := newWorld(t)
		c := w.check("data_dir")
		want(t, c, api.DoctorStatusOK, codeDataDirOK, "")
		if c.Message != w.dataDir+" (0700, 37.2 GB free)" {
			t.Errorf("message %q", c.Message)
		}
		if _, ok := c.Params["uid"]; ok {
			t.Error("a result that is fine carries the machine's uid")
		}
	})
	t.Run("not created yet", func(t *testing.T) {
		w := newWorld(t)
		w.dataDir = filepath.Join(t.TempDir(), "never-started")
		w.live = nil
		want(t, w.check("data_dir"), api.DoctorStatusInfo, codeDataDirNotCreated, "")
		// A running server whose directory is gone is another matter.
		w.live = healthyStatus()
		want(t, w.check("data_dir"), api.DoctorStatusFail, codeDataDirMissing, "")
	})
	t.Run("not a directory", func(t *testing.T) {
		w := newWorld(t)
		w.dataDir = filepath.Join(w.dataDir, "secrets.json")
		want(t, w.check("data_dir"), api.DoctorStatusFail, codeDataDirNotDir, fixDataDirNotDir)
	})
	t.Run("open to others", func(t *testing.T) {
		w := newWorld(t)
		if err := os.Chmod(w.dataDir, 0o755); err != nil { //nolint:gosec // G302: the case under test
			t.Fatal(err)
		}
		c := w.check("data_dir")
		want(t, c, api.DoctorStatusWarn, codeDataDirModeWide, fixDataDirMode)
		contains(t, c.Message, "has mode 0755")
		contains(t, c.Fix, "sudo chmod 700 "+w.dataDir)
	})
	t.Run("little space", func(t *testing.T) {
		w := newWorld(t)
		w.env.DiskFree = func(string) (uint64, error) { return 400_000_000, nil }
		c := w.check("data_dir")
		want(t, c, api.DoctorStatusWarn, codeDataDirLowSpace, fixDataDirSpace)
		contains(t, c.Message, "has only 400 MB free")
		// A filesystem that can't say how much is free is no finding.
		w.env.DiskFree = func(string) (uint64, error) { return 0, errors.ErrUnsupported }
		want(t, w.check("data_dir"), api.DoctorStatusOK, codeDataDirOK, "")
	})
	t.Run("another user's", func(t *testing.T) {
		// Inside the server the service's user is the one the process runs as; no user is looked up by name.
		w := newWorld(t)
		w.uid = os.Getuid() + 1000
		w.env.LookupUser = func(string) (int, bool) {
			t.Error("a running server looked its user up by name")
			return 0, false
		}
		c := w.check("data_dir")
		want(t, c, api.DoctorStatusFail, codeDataDirWrongOwner, fixDataDirOwner)
		if want := fmt.Sprintf("%s belongs to uid %d, but isshoni runs as uid %d", w.dataDir, os.Getuid(), w.uid); c.Message != want {
			t.Errorf("message:\n got %q\nwant %q", c.Message, want)
		}
		if want := "sudo chown -R isshoni " + w.dataDir; c.Fix != want {
			t.Errorf("fix:\n got %q\nwant %q", c.Fix, want)
		}
	})
	t.Run("offline on an installed host", func(t *testing.T) {
		// The service's user is the one named isshoni, whoever runs doctor: root (`sudo isshoni doctor`, the
		// command install.sh prints) or an ordinary user. A stopped server whose next start would exit 78 is
		// explained to both, and the directory is never given to the caller.
		owner := os.Getuid()
		for _, caller := range []struct {
			uid  int
			name string
		}{{0, "root"}, {owner + 1000, "alice"}} {
			w := newWorld(t)
			w.live, w.uid, w.env.User = nil, caller.uid, caller.name
			w.env.LookupUser = serviceUserIs(owner + 2000)
			c := w.check("data_dir")
			want(t, c, api.DoctorStatusFail, codeDataDirWrongOwner, fixDataDirOwner)
			if want := fmt.Sprintf("%s belongs to uid %d, but isshoni runs as the user isshoni (uid %d)", w.dataDir, owner, owner+2000); c.Message != want {
				t.Errorf("as %s, message:\n got %q\nwant %q", caller.name, c.Message, want)
			}
			if want := "sudo chown -R isshoni " + w.dataDir; c.Fix != want {
				t.Errorf("as %s, fix:\n got %q\nwant %q", caller.name, c.Fix, want)
			}
			if c.Params["user"] != ServiceUser || c.Params["uid"] != owner+2000 || c.Params["owner"] != owner {
				t.Errorf("as %s, params %v", caller.name, c.Params)
			}

			// The directory is the service's: fine, also for a caller who is someone else.
			w.env.LookupUser = serviceUserIs(owner)
			c = w.check("data_dir")
			want(t, c, api.DoctorStatusOK, codeDataDirOK, "")
			if _, ok := c.Params["user"]; ok {
				t.Errorf("as %s, a result that is fine names the service's user: %v", caller.name, c.Params)
			}
		}
		// And as the service's user itself (sudo -u isshoni isshoni doctor).
		w := newWorld(t)
		w.live = nil
		w.env.LookupUser = serviceUserIs(owner)
		want(t, w.check("data_dir"), api.DoctorStatusOK, codeDataDirOK, "")

		// A directory of the ordinary user who asks is a broken installation, or a second server that this user
		// starts by hand. doctor can't tell, so the fix says when it is not one.
		if owner == 0 {
			return // root's own directory: root should not be the one who runs the server
		}
		w.env.User = "alice"
		w.env.LookupUser = serviceUserIs(owner + 2000)
		c := w.check("data_dir")
		want(t, c, api.DoctorStatusFail, codeDataDirWrongOwner, fixDataDirOwner)
		if want := "sudo chown -R isshoni " + w.dataDir + "\n(unless you start this isshoni yourself as alice: then leave it as it is)"; c.Fix != want {
			t.Errorf("the caller's own directory, fix:\n got %q\nwant %q", c.Fix, want)
		}
		c = w.check("secrets")
		want(t, c, api.DoctorStatusFail, codeSecretsWrongOwner, fixSecretsOwner)
		contains(t, c.Fix, "sudo chown isshoni "+filepath.Join(w.dataDir, "secrets.json")+"\n(unless you start this isshoni yourself as alice: ")
	})
	t.Run("offline without a user named isshoni", func(t *testing.T) {
		// A development machine, or a setup made by hand: the caller is taken for the service's user. That is a
		// guess, so the fix does not hand the directory to the caller without saying when that is right.
		w := newWorld(t)
		w.live, w.uid, w.env.User = nil, os.Getuid()+1000, "alice"
		c := w.check("data_dir")
		want(t, c, api.DoctorStatusFail, codeDataDirWrongOwner, fixDataDirOwner)
		if want := fmt.Sprintf("%s belongs to uid %d, but doctor runs as uid %d", w.dataDir, os.Getuid(), w.uid); c.Message != want {
			t.Errorf("message:\n got %q\nwant %q", c.Message, want)
		}
		wantFix := fmt.Sprintf("if isshoni runs as the owner (uid %d), run doctor as that user: sudo -u '#%d' isshoni doctor\n"+
			"if it runs as alice: sudo chown -R alice %s", os.Getuid(), os.Getuid(), w.dataDir)
		if c.Fix != wantFix {
			t.Errorf("fix:\n got %q\nwant %q", c.Fix, wantFix)
		}
		// The caller's own directory is fine.
		w.uid = os.Getuid()
		want(t, w.check("data_dir"), api.DoctorStatusOK, codeDataDirOK, "")
		// Root is not taken for the service's user: there is nobody to compare the owner with.
		w.uid, w.env.User = 0, "root"
		want(t, w.check("data_dir"), api.DoctorStatusOK, codeDataDirOK, "")
	})
	t.Run("not writable", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("file modes don't keep root out")
		}
		w := newWorld(t)
		if err := os.Chmod(w.dataDir, 0o500); err != nil { //nolint:gosec // G302: the case under test
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(w.dataDir, 0o700) }) //nolint:gosec // G302: so the temp dir can be removed
		c := w.check("data_dir")
		want(t, c, api.DoctorStatusFail, codeDataDirNotWritable, fixDataDirOwner)
		contains(t, c.Message, "is not writable for isshoni (permission denied)")

		// Offline as the service's user the answer is the same; as anyone else the kernel is not asked, because
		// what this process may write says nothing about the service.
		w.live = nil
		w.env.LookupUser = serviceUserIs(os.Getuid())
		c = w.check("data_dir")
		want(t, c, api.DoctorStatusFail, codeDataDirNotWritable, fixDataDirOwner)
		if want := "sudo chown -R isshoni " + w.dataDir; c.Fix != want || c.Params["user"] != ServiceUser {
			t.Errorf("fix %q, params %v; want %q", c.Fix, c.Params, want)
		}
		w.uid, w.env.User = os.Getuid()+1000, "alice"
		want(t, w.check("data_dir"), api.DoctorStatusOK, codeDataDirOK, "")
	})
	t.Run("container", func(t *testing.T) {
		w := newWorld(t)
		w.container("docker", true)
		want(t, w.check("data_dir"), api.DoctorStatusOK, codeDataDirOK, "")

		w.container("docker", false)
		c := w.check("data_dir")
		want(t, c, api.DoctorStatusFail, codeDataDirNotMounted, fixDataDirMount)
		contains(t, c.Message, "is not a mounted volume: your data would be lost when the container is removed")
		contains(t, c.Fix, "Mount a volume at "+w.dataDir+" (see compose.yaml), e.g. -v isshoni-data:"+w.dataDir)

		// The test-only switch turns the failure into a warning.
		w.env.Host.Environ = append(w.env.Host.Environ, config.EnvAllowEphemeralData+"=1")
		want(t, w.check("data_dir"), api.DoctorStatusWarn, codeDataDirEphemeralAllowed, fixDataDirMount)

		// A mount table that can't be read fails like at startup (04 §5.1).
		w.env.Host = config.Host{Environ: []string{"container=podman"}, Root: t.TempDir()}
		c = w.check("data_dir")
		want(t, c, api.DoctorStatusFail, codeDataDirNotMounted, fixDataDirMount)
		contains(t, c.Message, "can't check that")

		// Offline in a container the caller is the image's user, which the service runs as too: no user is looked
		// up by name, and the fix is for the container's host.
		w = newWorld(t)
		w.container("docker", true)
		w.live, w.uid = nil, os.Getuid()+1000
		w.env.LookupUser = func(string) (int, bool) {
			t.Error("a user was looked up by name in a container")
			return 0, false
		}
		c = w.check("data_dir")
		want(t, c, api.DoctorStatusFail, codeDataDirWrongOwner, fixDataDirOwner)
		contains(t, c.Message, fmt.Sprintf("but doctor runs as uid %d", w.uid))
		if want := fmt.Sprintf("on the container's host, run: sudo chown -R %d:%d <the host path of %s>", w.uid, w.uid, w.dataDir); c.Fix != want {
			t.Errorf("fix:\n got %q\nwant %q", c.Fix, want)
		}
	})
	t.Run("doctor writes nothing", func(t *testing.T) {
		w := newWorld(t)
		w.live = nil
		before := listDir(t, w.dataDir)
		Run(t.Context(), w.build(), api.BandwidthInput{}, []string{"data_dir", "secrets", "schema"})
		if after := listDir(t, w.dataDir); !slices.Equal(before, after) {
			t.Errorf("the data directory changed: %v, was %v", after, before)
		}
	})
}

// listDir returns the names in dir.
func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names
}

func TestCheckSecrets(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		w := newWorld(t)
		c := w.check("secrets")
		want(t, c, api.DoctorStatusOK, codeSecretsOK, "")
		if c.Message != filepath.Join(w.dataDir, "secrets.json")+" (0600)" {
			t.Errorf("message %q", c.Message)
		}
	})
	t.Run("missing", func(t *testing.T) {
		w := newWorld(t)
		path := filepath.Join(w.dataDir, "secrets.json")
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		// Before the first start there is nothing yet.
		w.live = nil
		want(t, w.check("secrets"), api.DoctorStatusInfo, codeSecretsNotCreated, "")
		// Next to a database the keys are missing.
		if err := os.WriteFile(filepath.Join(w.dataDir, "isshoni.db"), []byte("db"), 0o600); err != nil {
			t.Fatal(err)
		}
		c := w.check("secrets")
		want(t, c, api.DoctorStatusFail, codeSecretsMissing, fixSecretsRestore)
		contains(t, c.Message, "sign everyone out")
		contains(t, c.Fix, "sudo -u isshoni isshoni admin restore --offline <backup>")
		// And under a running server too.
		w.live = healthyStatus()
		want(t, w.check("secrets"), api.DoctorStatusFail, codeSecretsMissing, fixSecretsRestore)
	})
	t.Run("corrupt", func(t *testing.T) {
		w := newWorld(t)
		if err := os.WriteFile(filepath.Join(w.dataDir, "secrets.json"), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		c := w.check("secrets")
		want(t, c, api.DoctorStatusFail, codeSecretsCorrupt, fixSecretsRestore)
		contains(t, c.Message, "can't be used (", "isshoni never replaces it")
		w.container("docker", true)
		contains(t, w.check("secrets").Fix, "docker compose run --rm isshoni admin restore --offline <backup>")
	})
	t.Run("open to others", func(t *testing.T) {
		w := newWorld(t)
		if err := os.Chmod(filepath.Join(w.dataDir, "secrets.json"), 0o640); err != nil { //nolint:gosec // G302: the case under test
			t.Fatal(err)
		}
		c := w.check("secrets")
		want(t, c, api.DoctorStatusWarn, codeSecretsModeWide, fixSecretsMode)
		contains(t, c.Message, "has mode 0640; isshoni makes it 0600 at its next start")
	})
	t.Run("another user's", func(t *testing.T) {
		w := newWorld(t)
		path := filepath.Join(w.dataDir, "secrets.json")
		w.uid = os.Getuid() + 1000
		c := w.check("secrets")
		want(t, c, api.DoctorStatusFail, codeSecretsWrongOwner, fixSecretsOwner)
		contains(t, c.Message, fmt.Sprintf("but isshoni runs as uid %d", w.uid))
		contains(t, c.Fix, "sudo chown isshoni ")

		// `sudo isshoni doctor` on a stopped server (04 §6.3): root compares the owner with the service's user, so
		// a file that makes the next start exit 78 (secrets_owner) is not reported as fine.
		w.live, w.uid, w.env.User = nil, 0, "root"
		w.env.LookupUser = serviceUserIs(os.Getuid() + 2000)
		c = w.check("secrets")
		want(t, c, api.DoctorStatusFail, codeSecretsWrongOwner, fixSecretsOwner)
		if want := fmt.Sprintf("%s belongs to uid %d, but isshoni runs as the user isshoni (uid %d)", path, os.Getuid(), os.Getuid()+2000); c.Message != want {
			t.Errorf("message:\n got %q\nwant %q", c.Message, want)
		}
		if want := "sudo chown isshoni " + path; c.Fix != want {
			t.Errorf("fix:\n got %q\nwant %q", c.Fix, want)
		}
		w.env.LookupUser = serviceUserIs(os.Getuid())
		want(t, w.check("secrets"), api.DoctorStatusOK, codeSecretsOK, "")

		// Without a user named isshoni the caller is taken for the service's user, as a guess; root is not.
		w.env.LookupUser = noServiceUser
		want(t, w.check("secrets"), api.DoctorStatusOK, codeSecretsOK, "")
		w.uid, w.env.User = os.Getuid()+1000, "alice"
		c = w.check("secrets")
		want(t, c, api.DoctorStatusFail, codeSecretsWrongOwner, fixSecretsOwner)
		contains(t, c.Message, fmt.Sprintf("but doctor runs as uid %d", w.uid))
		contains(t, c.Fix, fmt.Sprintf("if isshoni runs as the owner (uid %d), run doctor as that user: sudo -u '#%d' isshoni doctor\n", os.Getuid(), os.Getuid()),
			"if it runs as alice: sudo chown alice "+path)
	})
	t.Run("unreadable", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("file modes don't keep root out")
		}
		w := newWorld(t)
		path := filepath.Join(w.dataDir, "secrets.json")
		if err := os.Chmod(path, 0o000); err != nil {
			t.Fatal(err)
		}
		c := w.check("secrets")
		want(t, c, api.DoctorStatusFail, codeSecretsUnreadable, fixSecretsOwner)
		contains(t, c.Message, "permission denied")
		// Offline, an ordinary user is told to ask as root.
		w.live = nil
		want(t, w.check("secrets"), api.DoctorStatusFail, codeSecretsUnreadable, fixSecretsSudo)
	})
}

func TestCheckSchema(t *testing.T) {
	// With a server: its own schema version, which is this binary's.
	w := newWorld(t)
	c := w.check("schema")
	want(t, c, api.DoctorStatusOK, codeSchemaOK, "")
	if c.Message != "schema 7" {
		t.Errorf("message %q", c.Message)
	}
	w.live.SchemaVersion = 0 // a status without it
	want(t, w.check("schema"), api.DoctorStatusSkip, codeSkipNotReported, "")

	// Offline: through DBFiles, on the database file.
	offline := func(t *testing.T, info DBInfo, err error) *world {
		t.Helper()
		w := newWorld(t)
		w.live = nil
		db := filepath.Join(w.dataDir, "isshoni.db")
		if werr := os.WriteFile(db, []byte("SQLite format 3\x00"), 0o600); werr != nil {
			t.Fatal(werr)
		}
		w.env.DB.Inspect = func(_ context.Context, path string) (DBInfo, error) {
			if path != db {
				t.Errorf("Inspect(%q), want %q", path, db)
			}
			return info, err
		}
		return w
	}
	t.Run("offline ok", func(t *testing.T) {
		w := offline(t, DBInfo{SchemaVersion: 7, LastAppVersion: "0.3.0", HistoryOK: true}, nil)
		c := w.check("schema")
		want(t, c, api.DoctorStatusOK, codeSchemaOK, "")
		contains(t, c.Message, "schema 7 (", "isshoni.db)")
	})
	t.Run("no database yet", func(t *testing.T) {
		w := newWorld(t)
		w.live = nil
		w.env.DB.Inspect = func(context.Context, string) (DBInfo, error) {
			t.Error("Inspect was called without a database file")
			return DBInfo{}, nil
		}
		want(t, w.check("schema"), api.DoctorStatusInfo, codeSchemaNoDB, "")
	})
	t.Run("no store functions", func(t *testing.T) {
		w := newWorld(t)
		w.live = nil
		want(t, w.check("schema"), api.DoctorStatusSkip, codeSkipNotReported, "")
	})
	t.Run("older", func(t *testing.T) {
		w := offline(t, DBInfo{SchemaVersion: 5, LastAppVersion: "0.2.0", HistoryOK: true}, nil)
		c := w.check("schema")
		want(t, c, api.DoctorStatusOK, codeSchemaOlder, "")
		contains(t, c.Message, "schema 5; this isshoni brings it to 7 at its next start, after a backup")
	})
	t.Run("newer with a backup", func(t *testing.T) {
		const storeMessage = "database schema 9 is newer than this isshoni build supports (7); it was last used by " +
			"isshoni 0.6.1. Install isshoni 0.6.1 or newer, or restore the database from before the upgrade: " +
			"/var/lib/isshoni/backups/pre-7-20261014T021500Z.db (changes made after that backup are lost)"
		w := offline(t, DBInfo{
			SchemaVersion: 9, LastAppVersion: "0.6.1", HistoryOK: true,
			TooNewBackup: "/var/lib/isshoni/backups/pre-7-20261014T021500Z.db", TooNewMessage: storeMessage,
		}, nil)
		c := w.check("schema")
		want(t, c, api.DoctorStatusFail, codeSchemaNewer, fixSchemaRestore)
		contains(t, c.Message, "database schema 9 is newer than this isshoni build supports (7): isshoni refuses to start")
		// The fix is 03's message and the restore command for the environment (04 §13.2).
		wantFix := storeMessage + "\nsudo -u isshoni isshoni admin restore --offline " +
			"/var/lib/isshoni/backups/pre-7-20261014T021500Z.db && sudo systemctl start isshoni"
		if c.Fix != wantFix {
			t.Errorf("fix:\n got %q\nwant %q", c.Fix, wantFix)
		}
		w.container("docker", true)
		contains(t, w.check("schema").Fix, "docker compose stop && docker compose run --rm isshoni admin restore --offline "+
			"/var/lib/isshoni/backups/pre-7-20261014T021500Z.db && docker compose up -d")
	})
	t.Run("newer without a backup", func(t *testing.T) {
		w := offline(t, DBInfo{SchemaVersion: 9, LastAppVersion: "0.6.1", HistoryOK: true}, nil)
		c := w.check("schema")
		want(t, c, api.DoctorStatusFail, codeSchemaNewer, fixSchemaUpgrade)
		contains(t, c.Fix, "Install isshoni 0.6.1 or newer", "no backup from before the upgrade was found")
	})
	t.Run("another build's history", func(t *testing.T) {
		w := offline(t, DBInfo{SchemaVersion: 7, LastAppVersion: "0.3.0-fork", HistoryOK: false}, nil)
		c := w.check("schema")
		want(t, c, api.DoctorStatusFail, codeSchemaHistoryMismatch, fixSchemaSameBuild)
		contains(t, c.Fix, "install isshoni 0.3.0-fork")
	})
	t.Run("corrupt", func(t *testing.T) {
		w := offline(t, DBInfo{SchemaVersion: 7, HistoryOK: true, Integrity: errors.New("row 12 missing from index users_name")}, nil)
		c := w.check("schema")
		want(t, c, api.DoctorStatusFail, codeSchemaCorrupt, fixSchemaRestoreBackup)
		contains(t, c.Message, "failed its integrity check (row 12 missing from index users_name)")
	})
	t.Run("unreadable", func(t *testing.T) {
		w := offline(t, DBInfo{}, errors.New("file is not a database"))
		c := w.check("schema")
		want(t, c, api.DoctorStatusFail, codeSchemaUnreadable, fixSchemaRestoreBackup)
		contains(t, c.Message, "file is not a database")
		// An ordinary user who may not read the file is told to ask as root.
		w = offline(t, DBInfo{}, &fs.PathError{Op: "open", Path: "isshoni.db", Err: fs.ErrPermission})
		w.uid = max(os.Getuid(), 1)
		want(t, w.check("schema"), api.DoctorStatusFail, codeSchemaUnreadable, fixSchemaSudo)
	})
}

// The restore command of 04 §6.1 step 4, in both forms; the server prints the same one when it refuses to start.
func TestRestoreCommand(t *testing.T) {
	const backup = "/var/lib/isshoni/backups/pre-4-20261014T021500Z.db"
	if got, want := RestoreCommand(backup, false), "sudo -u isshoni isshoni admin restore --offline "+backup+" && sudo systemctl start isshoni"; got != want {
		t.Errorf("systemd form:\n got %s\nwant %s", got, want)
	}
	if got, want := RestoreCommand(backup, true), "docker compose stop && docker compose run --rm isshoni admin restore --offline "+backup+" && docker compose up -d"; got != want {
		t.Errorf("Docker form:\n got %s\nwant %s", got, want)
	}
	if got := RestoreCommand("/data dir/it's.db", false); !strings.Contains(got, `--offline '/data dir/it'\''s.db' &&`) {
		t.Errorf("a path with spaces and a quote: %s", got)
	}
}
