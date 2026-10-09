package ops

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/version"
)

// restarts records what an admin server asked its server for.
type restarts struct {
	mu   sync.Mutex
	why  []RestartReason
	hook func() // runs inside Restart, when set
}

func (r *restarts) restart(why RestartReason) {
	r.mu.Lock()
	r.why = append(r.why, why)
	hook := r.hook
	r.mu.Unlock()
	if hook != nil {
		hook()
	}
}

func (r *restarts) asked() []RestartReason {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.why)
}

// dataAdmin is an admin socket over the data directory of a test, as the wiring builds it for a running server:
// with the database's snapshot, the real secrets.json, and a Restart that only takes note.
type dataAdmin struct {
	*testAdmin
	data     *testData
	secrets  *config.SecretStore
	restarts *restarts
}

func startDataAdmin(t *testing.T, db fakeDB) *dataAdmin {
	t.Helper()
	td := newTestData(t, db).online()
	secrets, err := config.OpenSecrets(td.Paths.Secrets, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := &dataAdmin{data: td, secrets: secrets, restarts: &restarts{}}
	d.testAdmin = startAdmin(t, func(o *AdminOptions, _ *AdminListenOptions) {
		td.Logger = o.Logger
		o.Data, o.Secrets, o.Restart = td.DataDir, secrets, d.restarts.restart
	})
	td.logs = d.logs
	return d
}

// GET /v1/backup streams the archive of 04 §12.3 with the name the file should get.
func TestAdminBackup(t *testing.T) {
	a := startDataAdmin(t, fakeDB{Schema: 6, App: "0.2.9", Note: "alpha"})
	code, hdr, body := a.raw(t, http.MethodGet, "/v1/backup?certs=1", "")
	wantName := "isshoni-backup-" + version.Version() + "-20260929T101500Z.tar.gz"
	if code != 200 || hdr.Get("Content-Type") != "application/gzip" ||
		hdr.Get("Content-Disposition") != `attachment; filename=`+wantName && hdr.Get("Content-Disposition") != `attachment; filename="`+wantName+`"` {
		t.Fatalf("GET /v1/backup = %d, Content-Type %q, Content-Disposition %q", code, hdr.Get("Content-Type"), hdr.Get("Content-Disposition"))
	}
	m, entries := readArchive(t, []byte(body))
	if m.SchemaVersion != 6 || !slices.Equal(m.Contents, []string{"isshoni.db", "secrets.json", "certmagic/"}) || len(entries) < 5 {
		t.Errorf("the archive: manifest %+v, entries %v", m, names(entries))
	}

	// The client: the stream and the proposed name; without certificates when asked.
	b, err := a.client.Backup(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(b)
	if err != nil || b.Filename != wantName {
		t.Fatalf("Backup: %v, Filename %q", err, b.Filename)
	}
	_ = b.Close()
	if m, entries := readArchive(t, data); !slices.Equal(m.Contents, []string{"isshoni.db", "secrets.json"}) || len(entries) != 2 {
		t.Errorf("certs=0: manifest %+v, entries %v", m, names(entries))
	}
	// No certs parameter means with certificates; anything else is a bad request.
	if _, _, body := a.raw(t, http.MethodGet, "/v1/backup", ""); !bytes.Contains([]byte(body), []byte{0x1f, 0x8b}) {
		t.Error("GET /v1/backup without certs= is no archive")
	} else if m, _ := readArchive(t, []byte(body)); !slices.Contains(m.Contents, "certmagic/") {
		t.Errorf("GET /v1/backup without certs= holds %v, want the certificates too", m.Contents)
	}
	if code, _, body := a.raw(t, http.MethodGet, "/v1/backup?certs=yes", ""); code != 400 || !strings.Contains(body, `"bad_request"`) {
		t.Errorf("certs=yes = %d %s", code, body)
	}
	// HEAD says what a GET would send and stages nothing.
	if code, hdr, body := a.raw(t, http.MethodHead, "/v1/backup", ""); code != 200 || body != "" || hdr.Get("Content-Type") != "application/gzip" {
		t.Errorf("HEAD /v1/backup = %d %q %q", code, hdr.Get("Content-Type"), body)
	}
	if left := tree(t, a.data.Paths.Restore, nil); len(left) != 0 {
		t.Errorf("restore/ after the backups: %v", keys(left))
	}
	// One info line per backup, with the peer's uid where the platform gives one; a backup is not audited.
	logs := a.logs.String()
	if n := strings.Count(logs, `"msg":"backup written through the admin socket"`); n != 3 {
		t.Errorf("%d log lines for 3 backups:\n%s", n, logs)
	}
	if peerCredSupported && !strings.Contains(logs, fmt.Sprintf(`"peer_uid":%d`, os.Getuid())) {
		t.Errorf("the backup's log line has no peer uid:\n%s", logs)
	}
	if len(a.restarts.asked()) != 0 {
		t.Errorf("a backup asked for a restart: %v", a.restarts.asked())
	}
}

// A backup that can't be made is an error answer, never a partial archive with status 200.
func TestAdminBackupErrors(t *testing.T) {
	a := startDataAdmin(t, fakeDB{Schema: 7, Note: "alpha"})
	a.data.Snapshot = func(context.Context, string) error { return fmt.Errorf("VACUUM INTO: %w", syscall.ENOSPC) }
	_, err := a.client.Backup(t.Context(), true)
	if ae := adminError(t, err, api.CodeInsufficientStorage); ae.Status != 507 || !strings.Contains(ae.Message, "free disk space") || ae.Fix == "" {
		t.Errorf("a backup on a full disk: %+v", ae)
	}
	a.data.Snapshot = func(context.Context, string) error { return errors.New("database is locked") }
	_, err = a.client.Backup(t.Context(), true)
	if ae := adminError(t, err, api.CodeInternal); strings.Contains(ae.Message, "locked") || ae.API.RequestID == "" {
		t.Errorf("a backup that fails: %+v, want an internal error with a ref and nothing of the cause", ae)
	}
	if !strings.Contains(a.logs.String(), "database is locked") {
		t.Errorf("the cause is not in the log:\n%s", a.logs.String())
	}

}

// onMessage is a log handler that calls do when a record with the message msg passes.
type onMessage struct {
	slog.Handler
	msg string
	do  func()
}

func (h onMessage) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == h.msg {
		h.do()
	}
	return h.Handler.Handle(ctx, r)
}

func (h onMessage) WithAttrs(attrs []slog.Attr) slog.Handler {
	return onMessage{h.Handler.WithAttrs(attrs), h.msg, h.do}
}

// An archive that breaks off after its status went out ends the connection without ending the body: the client
// gets an error while it reads, never a short archive that looks whole.
func TestAdminBackupBreaksOff(t *testing.T) {
	// A database large enough that a part of the archive leaves before all of it is read: random, so that it does
	// not compress to nothing.
	noise := make([]byte, 1<<20)
	_, _ = rand.Read(noise)
	td := newTestData(t, fakeDB{Schema: 7, Note: hex.EncodeToString(noise)}).online()
	var harm func(snapshot string)
	a := startAdmin(t, func(o *AdminOptions, _ *AdminListenOptions) {
		o.Data = td.DataDir
		// The handler logs its line between staging the backup and writing it: there the snapshot comes to harm.
		o.Logger = slog.New(onMessage{o.Logger.Handler(), "backup written through the admin socket", func() {
			snaps, _ := filepath.Glob(filepath.Join(td.Paths.Restore, "snapshot-*.db"))
			for _, s := range snaps {
				harm(s)
			}
		}})
	})

	// The snapshot is cut short under the handler: half of the database leaves, then the archive breaks off.
	harm = func(snapshot string) { _ = os.Truncate(snapshot, 1<<20) }
	b, err := a.client.Backup(t.Context(), true)
	if err != nil {
		t.Fatalf("Backup: %v, want the stream and then a read error", err)
	}
	data, err := io.ReadAll(b)
	_ = b.Close()
	if err == nil || len(data) == 0 {
		t.Errorf("reading a backup that broke off: %d bytes, error %v; want a part and then an error", len(data), err)
	}
	if !strings.Contains(a.logs.String(), "the backup could not be sent to its end") {
		t.Errorf("the log does not say that the backup broke off:\n%s", a.logs.String())
	}

	// The snapshot is gone before a byte has left: that is still an ordinary error answer.
	harm = func(snapshot string) { _ = os.Remove(snapshot) }
	_, err = a.client.Backup(t.Context(), true)
	if ae := adminError(t, err, api.CodeInternal); ae.API.RequestID == "" {
		t.Errorf("a backup that fails before its first byte: %+v", ae)
	}

	// The server goes on, and the next backup is whole.
	harm = func(string) {}
	b, err = a.client.Backup(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	data, err = io.ReadAll(b)
	_ = b.Close()
	if m, _ := readArchive(t, data); err != nil || m.SchemaVersion != 7 {
		t.Errorf("the backup after the broken ones: %v, manifest %+v", err, m)
	}
}

// POST /v1/restore validates, backs the current state up, writes the plan, answers 202 and then asks for the
// restart (04 §12.4). The files are not swapped by the handler: the server does that at the end of its shutdown.
func TestAdminRestore(t *testing.T) {
	for _, tt := range []struct {
		name, contentType string
		kind              RestoreKind
		body              func(t *testing.T) []byte
	}{
		{"archive", AdminContentArchive, RestoreArchive, func(t *testing.T) []byte {
			return archiveOf(t, newTestData(t, fakeDB{Schema: 6, App: "0.2.9", Note: "alpha"}), BackupOptions{})
		}},
		{"database file", AdminContentSQLite, RestoreDatabase, func(*testing.T) []byte {
			return fakeDB{Schema: 6, App: "0.2.9", Note: "alpha"}.bytes()
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a := startDataAdmin(t, fakeDB{Schema: latestSchema, Note: "beta"})
			before := a.data.serverFiles(t)
			// The answer must be on its way when the restart is asked for.
			answered := make(chan struct{})
			a.restarts.hook = func() {
				select {
				case <-answered:
				case <-time.After(5 * time.Second):
					t.Error("the restart was asked for before the client had its answer")
				}
			}
			body := tt.body(t)
			var res AdminRestoreResult
			var err error
			go func() {
				defer close(answered)
				res, err = a.client.Restore(t.Context(), bytes.NewReader(body), tt.contentType)
			}()
			<-answered
			if err != nil || !res.Restarting || res.PreRestoreBackup != "backups/pre-restore-20260929T101500Z.tar.gz" {
				t.Fatalf("Restore = %+v, %v", res, err)
			}
			waitFor(t, func() bool { return len(a.restarts.asked()) == 1 })
			if got := a.restarts.asked(); !slices.Equal(got, []RestartReason{RestartAfterRestore}) {
				t.Errorf("restarts asked for: %v", got)
			}
			if now := a.data.serverFiles(t); !reflect.DeepEqual(now, before) {
				t.Errorf("the handler changed the server's files; the swap is the shutdown's:\n got %v\nwant %v", keys(now), keys(before))
			}
			if plan := planOf(t, a.data); plan == nil || plan.State != "swapping" {
				t.Errorf("plan = %+v, want swapping", plan)
			}
			if m, _ := readArchive(t, []byte(a.data.read(t, res.PreRestoreBackup))); m.SchemaVersion != latestSchema {
				t.Errorf("the pre-restore backup: %+v", m)
			}
			logs := a.logs.String()
			if !strings.Contains(logs, `"msg":"restore accepted through the admin socket; restarting to swap the files in"`) ||
				!strings.Contains(logs, `"kind":"`+string(tt.kind)+`"`) ||
				(peerCredSupported && !strings.Contains(logs, fmt.Sprintf(`"peer_uid":%d`, os.Getuid()))) {
				t.Errorf("the restore's log line:\n%s", logs)
			}

			// The server is on its way down: nothing else is begun.
			_, err = a.client.Restore(t.Context(), bytes.NewReader(body), tt.contentType)
			wantCode(t, err, api.CodeServerShutdown)
			_, err = a.client.RotateSecrets(t.Context(), AdminRotateRequest{VAPID: true})
			wantCode(t, err, api.CodeServerShutdown)
			if got := a.restarts.asked(); len(got) != 1 {
				t.Errorf("restarts asked for after the refusals: %v", got)
			}
			// What the shutdown then does.
			if done, err := CompleteRestore(a.data.Paths, a.data.Logger); err != nil || !done {
				t.Fatalf("CompleteRestore = %v, %v", done, err)
			}
			if d, err := readFakeDB(a.data.Paths.DB); err != nil || d.Note != "alpha" {
				t.Errorf("the database after the swap: %+v, %v", d, err)
			}
		})
	}
}

// What the server refuses, with the code of 04 §12.2 and a text for the operator; the server goes on as it was.
func TestAdminRestoreRefusals(t *testing.T) {
	a := startDataAdmin(t, fakeDB{Schema: latestSchema, Note: "beta"})
	before := tree(t, a.data.dir, nil)
	good := goodEntries(t)
	for name, tt := range map[string]struct {
		body []byte
		code string
		text string
	}{
		"garbage":            {[]byte("not a backup"), api.CodeBackupInvalid, "neither a backup archive"},
		"empty":              {nil, api.CodeBackupInvalid, "it is empty"},
		"a bad checksum":     {makeArchive(t, manifestFor(good[1], tarEntry{name: "isshoni.db", body: "x"}), good[:2]...), api.CodeBackupInvalid, "checksum"},
		"a newer database":   {fakeDB{Schema: latestSchema + 1, App: "0.5.0"}.bytes(), api.CodeBackupNewer, "This backup is from isshoni 0.5.0; install 0.5.0 or newer first"},
		"a damaged database": {fakeDB{Schema: latestSchema, Damage: "page 3"}.bytes(), api.CodeBackupInvalid, "damaged"},
	} {
		_, err := a.client.Restore(t.Context(), bytes.NewReader(tt.body), AdminContentArchive)
		ae := adminError(t, err, tt.code)
		if !strings.Contains(ae.Message, tt.text) {
			t.Errorf("%s: message %q, want %q in it", name, ae.Message, tt.text)
		}
	}
	// The content type counts for nothing: the body says what it is.
	code, _, body := a.raw(t, http.MethodPost, "/v1/restore", `{"not":"a backup"}`)
	if code != 400 || !strings.Contains(body, `"backup_invalid"`) {
		t.Errorf("a JSON body = %d %s", code, body)
	}
	if after := tree(t, a.data.dir, func(rel string) bool { return rel == "restore" }); !reflect.DeepEqual(after, before) {
		t.Errorf("refused restores changed the data directory: %v", keys(after))
	}
	if left := tree(t, a.data.Paths.Restore, nil); len(left) != 0 {
		t.Errorf("refused restores left %v in restore/", keys(left))
	}
	if got := a.restarts.asked(); len(got) != 0 {
		t.Errorf("a refused restore asked for a restart: %v", got)
	}
	// A refusal leaves the server free for the next restore.
	if _, err := a.client.Restore(t.Context(), bytes.NewReader(fakeDB{Schema: latestSchema, Note: "ok"}.bytes()), AdminContentSQLite); err != nil {
		t.Errorf("a good restore after refused ones: %v", err)
	}

	// The pre-restore backup can't be written: the restore is refused, with nothing staged.
	b := startDataAdmin(t, fakeDB{Schema: latestSchema, Note: "beta"})
	b.data.Snapshot = func(context.Context, string) error { return fmt.Errorf("VACUUM INTO: %w", syscall.ENOSPC) }
	_, err := b.client.Restore(t.Context(), bytes.NewReader(fakeDB{Schema: latestSchema}.bytes()), AdminContentSQLite)
	if ae := adminError(t, err, api.CodeInsufficientStorage); ae.Status != 507 {
		t.Errorf("a restore on a full disk: %+v", ae)
	}
	if left := tree(t, b.data.Paths.Restore, nil); len(left) != 0 || len(b.restarts.asked()) != 0 {
		t.Errorf("after the refusal: restore/ %v, restarts %v", keys(left), b.restarts.asked())
	}
}

// Two restores at once: the second gets 409 restore_in_progress while the first is at work.
func TestAdminRestoreInProgress(t *testing.T) {
	a := startDataAdmin(t, fakeDB{Schema: latestSchema, Note: "beta"})
	// The first restore's body arrives in two parts, with the second restore between them.
	pr, pw := io.Pipe()
	type result struct {
		res AdminRestoreResult
		err error
	}
	first := make(chan result, 1)
	go func() {
		res, err := a.client.Restore(t.Context(), pr, AdminContentSQLite)
		first <- result{res, err}
	}()
	body := fakeDB{Schema: latestSchema, Note: "first"}.bytes()
	if _, err := pw.Write(body[:10]); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return exists(filepath.Join(a.data.Paths.Restore, "incoming")) })

	_, err := a.client.Restore(t.Context(), bytes.NewReader(fakeDB{Schema: latestSchema, Note: "second"}.bytes()), AdminContentSQLite)
	if ae := adminError(t, err, api.CodeRestoreInProgress); ae.Status != 409 || ae.Message == "" {
		t.Errorf("the second restore: %+v", ae)
	}
	_, err = a.client.RotateSecrets(t.Context(), AdminRotateRequest{Keys: []string{"session"}})
	wantCode(t, err, api.CodeRestoreInProgress)
	// A backup may still be taken: it only reads.
	if b, err := a.client.Backup(t.Context(), false); err != nil {
		t.Errorf("a backup during a restore: %v", err)
	} else {
		_, _ = io.Copy(io.Discard, b)
		_ = b.Close()
	}

	_, _ = pw.Write(body[10:])
	_ = pw.Close()
	if r := <-first; r.err != nil || !r.res.Restarting {
		t.Fatalf("the first restore = %+v, %v", r.res, r.err)
	}
	if _, err := CompleteRestore(a.data.Paths, a.data.Logger); err != nil {
		t.Fatal(err)
	}
	if d, _ := readFakeDB(a.data.Paths.DB); d.Note != "first" {
		t.Errorf("the database is %+v, want the first restore's", d)
	}
}

// POST /v1/rotate-secrets replaces exactly the named keys in secrets.json, answers 202 with their names and then
// asks for the restart (04 §5.3).
func TestAdminRotateSecrets(t *testing.T) {
	type keyDoc struct{ ID, Key string }
	type doc struct {
		Keys  map[string]keyDoc
		VAPID struct {
			PublicKey string `json:"public_key"`
		}
	}
	readSecrets := func(a *dataAdmin) (d doc) {
		t.Helper()
		if err := json.Unmarshal([]byte(a.data.read(t, "secrets.json")), &d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	for _, tt := range []struct {
		name    string
		in      AdminRotateRequest
		rotated []string
	}{
		{"all four, as the CLI asks", AdminRotateRequest{Keys: []string{"session", "invite", "resume"}, VAPID: true}, []string{"session", "invite", "resume", "vapid"}},
		{"one key", AdminRotateRequest{Keys: []string{"invite"}}, []string{"invite"}},
		{"in any order, named twice", AdminRotateRequest{Keys: []string{"resume", "session", "resume"}}, []string{"session", "resume"}},
		{"the push keys alone", AdminRotateRequest{VAPID: true}, []string{"vapid"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a := startDataAdmin(t, fakeDB{Schema: latestSchema})
			before := readSecrets(a)
			res, err := a.client.RotateSecrets(t.Context(), tt.in)
			if err != nil || !res.Restarting || !slices.Equal(res.Rotated, tt.rotated) {
				t.Fatalf("RotateSecrets = %+v, %v; want %v", res, err, tt.rotated)
			}
			after := readSecrets(a)
			for name, old := range before.Keys {
				changed := after.Keys[name].Key != old.Key
				if want := slices.Contains(tt.rotated, name); changed != want {
					t.Errorf("key %s changed = %v, want %v", name, changed, want)
				} else if want && after.Keys[name].ID != "2" {
					t.Errorf("key %s has the id %q after its first rotation, want 2", name, after.Keys[name].ID)
				}
			}
			if changed := after.VAPID.PublicKey != before.VAPID.PublicKey; changed != slices.Contains(tt.rotated, "vapid") {
				t.Errorf("the VAPID pair changed = %v", changed)
			}
			waitFor(t, func() bool { return len(a.restarts.asked()) == 1 })
			if got := a.restarts.asked(); !slices.Equal(got, []RestartReason{RestartAfterRotation}) {
				t.Errorf("restarts asked for: %v", got)
			}
			if logs := a.logs.String(); !strings.Contains(logs, `"msg":"secrets rotated through the admin socket; restarting to apply them"`) {
				t.Errorf("the rotation's log line:\n%s", logs)
			}
			// No key is ever in the log or in the answer.
			for name, k := range after.Keys {
				if strings.Contains(a.logs.String(), k.Key) {
					t.Errorf("the new %s key is in the log", name)
				}
			}
			// A second rotation finds a server that is restarting.
			_, err = a.client.RotateSecrets(t.Context(), tt.in)
			wantCode(t, err, api.CodeServerShutdown)
		})
	}

	a := startDataAdmin(t, fakeDB{Schema: latestSchema})
	before := a.data.read(t, "secrets.json")
	for body, want := range map[string]string{
		`{"keys":["session","cookie"],"vapid":true}`: `there is no key named "cookie"; the keys are session, invite, resume`,
		`{"keys":[]}`:        "nothing to rotate",
		``:                   "nothing to rotate",
		`{"keys":"session"}`: "not the JSON this endpoint takes",
	} {
		code, _, answer := a.raw(t, http.MethodPost, "/v1/rotate-secrets", body)
		if code != 400 || !strings.Contains(answer, `"bad_request"`) || !strings.Contains(answer, strings.ReplaceAll(want, `"`, `\"`)) {
			t.Errorf("rotate-secrets %s = %d %s; want 400 with %q", body, code, answer, want)
		}
	}
	if a.data.read(t, "secrets.json") != before || len(a.restarts.asked()) != 0 {
		t.Errorf("refused rotations changed secrets.json or asked for a restart (%v)", a.restarts.asked())
	}
}

// failingRotator is a secrets.json that can't be written.
type failingRotator struct{}

func (failingRotator) Rotate(context.Context, []config.KeyName, bool) (config.RotateResult, error) {
	return config.RotateResult{}, errors.New("config: rotating secrets: read-only file system")
}

// A rotation that fails changes nothing and restarts nothing; the server takes the next one.
func TestAdminRotateSecretsFails(t *testing.T) {
	restarts := &restarts{}
	var rotator SecretRotator = failingRotator{}
	a := startAdmin(t, func(o *AdminOptions, _ *AdminListenOptions) {
		o.Secrets = rotatorFunc(func(ctx context.Context, names []config.KeyName, vapid bool) (config.RotateResult, error) {
			return rotator.Rotate(ctx, names, vapid)
		})
		o.Restart = restarts.restart
	})
	_, err := a.client.RotateSecrets(t.Context(), AdminRotateRequest{Keys: []string{"session"}})
	if ae := adminError(t, err, api.CodeInternal); strings.Contains(ae.Message, "read-only") {
		t.Errorf("the failed rotation's answer carries the cause: %+v", ae)
	}
	if len(restarts.asked()) != 0 {
		t.Errorf("a failed rotation asked for a restart: %v", restarts.asked())
	}
	rotator = rotatorFunc(func(_ context.Context, names []config.KeyName, vapid bool) (config.RotateResult, error) {
		return config.RotateResult{Rotated: names, VAPID: vapid}, nil
	})
	if res, err := a.client.RotateSecrets(t.Context(), AdminRotateRequest{Keys: []string{"session"}}); err != nil || !slices.Equal(res.Rotated, []string{"session"}) {
		t.Errorf("the rotation after the failed one = %+v, %v", res, err)
	}
}

type rotatorFunc func(context.Context, []config.KeyName, bool) (config.RotateResult, error)

func (f rotatorFunc) Rotate(ctx context.Context, names []config.KeyName, vapid bool) (config.RotateResult, error) {
	return f(ctx, names, vapid)
}

// A server whose wiring gave the socket no data directory, no secrets or no way to restart says so: it never
// changes files that only a restart makes whole again.
func TestAdminDataWithoutParts(t *testing.T) {
	td := newTestData(t, fakeDB{Schema: latestSchema}).online()
	secrets, err := config.OpenSecrets(td.Paths.Secrets, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := tree(t, td.dir, nil)
	for name, adjust := range map[string]func(o *AdminOptions){
		"nothing":    func(*AdminOptions) {},
		"no restart": func(o *AdminOptions) { o.Data, o.Secrets = td.DataDir, secrets },
		"no data":    func(o *AdminOptions) { o.Secrets, o.Restart = secrets, func(RestartReason) {} },
	} {
		a := startAdmin(t, func(o *AdminOptions, _ *AdminListenOptions) { adjust(o) })
		calls := map[string]func() error{
			"restore": func() error {
				_, err := a.client.Restore(t.Context(), bytes.NewReader(fakeDB{Schema: 1}.bytes()), AdminContentSQLite)
				return err
			},
		}
		if name != "no data" {
			calls["rotate-secrets"] = func() error {
				_, err := a.client.RotateSecrets(t.Context(), AdminRotateRequest{Keys: []string{"session"}})
				return err
			}
		}
		if name != "no restart" {
			calls["backup"] = func() error { _, err := a.client.Backup(t.Context(), true); return err }
		}
		for what, call := range calls {
			ae := adminError(t, call(), api.CodeInternal)
			if ae.Message == "" || strings.Contains(ae.Message, "ref ") {
				t.Errorf("%s, %s: message %q, want the reason, not an internal-error ref", name, what, ae.Message)
			}
		}
	}
	if after := tree(t, td.dir, nil); !reflect.DeepEqual(after, before) {
		t.Errorf("a server without the parts changed the data directory: %v", keys(after))
	}
}

// During a shutdown the three endpoints answer 503 like every other (04 §12.2).
func TestAdminDataDuringShutdown(t *testing.T) {
	a := startDataAdmin(t, fakeDB{Schema: latestSchema})
	a.health.SetShuttingDown()
	_, err := a.client.Backup(t.Context(), true)
	wantCode(t, err, api.CodeServerShutdown)
	_, err = a.client.Restore(t.Context(), bytes.NewReader(fakeDB{Schema: 1}.bytes()), AdminContentSQLite)
	wantCode(t, err, api.CodeServerShutdown)
	_, err = a.client.RotateSecrets(t.Context(), AdminRotateRequest{VAPID: true})
	wantCode(t, err, api.CodeServerShutdown)
	if len(a.restarts.asked()) != 0 || exists(a.data.Paths.Restore) {
		t.Error("a request during the shutdown did something")
	}
}
