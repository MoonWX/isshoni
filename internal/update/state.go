package update

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/MoonWX/isshoni/internal/logx"
)

// state.json in the updates folder (06 §8.7): the pending installer, the persisted key revocations and the time of
// the last check. It is written atomically (a temp file in the same folder, synced, then renamed over the old one),
// so a crash leaves the old file or the new one, never half of one; it is at most 16 KiB.
//
//	{"v": 1,
//	 "pending": {"version": "0.2.1", "file": "isshoni-desktop_0.2.1_windows_amd64_setup.exe", "size": 15204352,
//	             "sha256": "…", "expiresAt": "2027-06-01T10:00:00.000Z", "attempts": 0,
//	             "required": false, "notesUrl": "https://github.com/MoonWX/isshoni/releases/tag/v0.2.1"},
//	 "revokedKeys": [],
//	 "lastCheckAt": "2026-12-05T09:12:00.000Z"}
//
// required and notesUrl carry the manifest's verdict and release page to a later start, which shows the update as
// ready (or failed) before any check.

const (
	stateName    = "state.json"
	stateVersion = 1
	maxStateSize = 16 << 10
)

// stateFile is state.json, schema 1.
type stateFile struct {
	V           int         `json:"v"`
	Pending     *pendingRec `json:"pending"`
	RevokedKeys []string    `json:"revokedKeys"`
	LastCheckAt wireTime    `json:"lastCheckAt,omitzero"`
}

// pendingRec is a downloaded, verified installer waiting to be applied.
type pendingRec struct {
	Version   string   `json:"version"`
	File      string   `json:"file"` // a plain file name in the updates folder
	Size      int64    `json:"size"`
	SHA256    string   `json:"sha256"`
	ExpiresAt wireTime `json:"expiresAt"` // the manifest's
	Attempts  int      `json:"attempts"`  // incremented before each launch (06 §8.7's loop guard)
	Required  bool     `json:"required"`
	NotesURL  string   `json:"notesUrl,omitempty"`
}

func (st *stateFile) clone() *stateFile {
	c := *st
	if st.Pending != nil {
		p := *st.Pending
		c.Pending = &p
	}
	c.RevokedKeys = slices.Clone(st.RevokedKeys)
	return &c
}

// wireTime is M1's wire time form, RFC 3339 in UTC with milliseconds ("2026-12-05T09:12:00.000Z"); any RFC 3339 time
// is read.
type wireTime struct{ time.Time }

const wireTimeLayout = "2006-01-02T15:04:05.000Z"

func (t wireTime) String() string { return t.UTC().Format(wireTimeLayout) }

func (t wireTime) MarshalJSON() ([]byte, error) { return json.Marshal(t.String()) }

func (t *wireTime) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return err
	}
	t.Time = v
	return nil
}

func emptyState() *stateFile { return &stateFile{V: stateVersion, RevokedKeys: []string{}} }

// parseState reads state.json. A file of another schema version keeps only the revocations it lists, so that a
// revoked key stays revoked; an entry that breaks the rules is dropped (the pending record as a whole, a bad
// fingerprint alone). The second result says what was dropped, for the log.
func parseState(data []byte) (*stateFile, string, error) {
	if len(data) > maxStateSize {
		return nil, "", fmt.Errorf("state: %d bytes, above the %d-byte limit", len(data), maxStateSize)
	}
	var probe struct {
		V           int               `json:"v"`
		Pending     json.RawMessage   `json:"pending"`
		RevokedKeys []json.RawMessage `json:"revokedKeys"`
		LastCheckAt json.RawMessage   `json:"lastCheckAt"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, "", fmt.Errorf("state: %w", err)
	}
	st := emptyState()
	var dropped []string
	for _, raw := range probe.RevokedKeys {
		var fp string
		if json.Unmarshal(raw, &fp) != nil || !fingerprintRE.MatchString(fp) {
			dropped = append(dropped, "a revokedKeys entry")
			continue
		}
		if !slices.Contains(st.RevokedKeys, fp) && len(st.RevokedKeys) < MaxRevokedKeys {
			st.RevokedKeys = append(st.RevokedKeys, fp)
		}
	}
	if probe.V != stateVersion {
		return st, fmt.Sprintf("all but the revocations of schema %d", probe.V), nil
	}
	if len(probe.LastCheckAt) > 0 && string(probe.LastCheckAt) != "null" {
		_ = json.Unmarshal(probe.LastCheckAt, &st.LastCheckAt) // a bad time only loses the time
	}
	if len(probe.Pending) > 0 && string(probe.Pending) != "null" {
		var p pendingRec
		err := json.Unmarshal(probe.Pending, &p)
		if err == nil {
			err = p.check()
		}
		if err != nil {
			dropped = append(dropped, "the pending update ("+err.Error()+")")
		} else {
			st.Pending = &p
		}
	}
	if len(dropped) > 0 {
		return st, fmt.Sprint(dropped), nil
	}
	return st, "", nil
}

// check applies the manifest's rules to a pending record read back from disk: the file must be a plain name in the
// updates folder, never a path.
func (p *pendingRec) check() error {
	if _, err := canonical(p.Version); err != nil {
		return err
	}
	switch {
	case !nameRE.MatchString(p.File) || p.File == stateName:
		return fmt.Errorf("file %q", truncate(p.File))
	case p.Size <= 0 || p.Size > MaxArtifactSize:
		return fmt.Errorf("size %d", p.Size)
	case !sha256RE.MatchString(p.SHA256):
		return errors.New("sha256")
	case p.Attempts < 0:
		return fmt.Errorf("attempts %d", p.Attempts)
	case p.ExpiresAt.IsZero():
		return errors.New("no expiresAt")
	}
	return nil
}

// marshalState encodes st as state.json.
func marshalState(st *stateFile) ([]byte, error) {
	c := st.clone()
	c.V = stateVersion
	if c.RevokedKeys == nil {
		c.RevokedKeys = []string{}
	}
	data, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	data = append(data, '\n')
	if len(data) > maxStateSize {
		return nil, fmt.Errorf("state: %d bytes, above the %d-byte limit", len(data), maxStateSize)
	}
	return data, nil
}

// writeAtomic writes data to dir/name through a temp file in dir, synced and then renamed over name.
func writeAtomic(dir, name string, data []byte, rename func(oldpath, newpath string) error) (err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, name+".*.tmp")
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
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return rename(tmp, filepath.Join(dir, name))
}

// loadLocked returns the cached state, reading state.json the first time. A missing file is the empty state; a file
// that can't be read or parsed is logged and counts as empty (it is replaced at the next write). Call with stateMu.
func (u *Updater) loadLocked() *stateFile {
	if u.st != nil {
		return u.st
	}
	data, err := os.ReadFile(filepath.Join(u.dir, stateName))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		u.st = emptyState()
	case err != nil:
		u.log.Warn("can't read the update state; starting empty", logx.Err(err))
		u.st = emptyState()
	default:
		st, dropped, err := parseState(data)
		if err != nil {
			u.log.Warn("the update state is broken; starting empty", logx.Err(err))
			st = emptyState()
		} else if dropped != "" {
			u.log.Warn("parts of the update state dropped", "dropped", dropped)
		}
		u.st = st
	}
	return u.st
}

// saveLocked writes st and caches it. Call with stateMu.
func (u *Updater) saveLocked(st *stateFile) error {
	data, err := marshalState(st)
	if err != nil {
		return err
	}
	if err := writeAtomic(u.dir, stateName, data, u.env.sys.rename); err != nil {
		return fmt.Errorf("update: write %s: %w", stateName, err)
	}
	u.st = st
	return nil
}

// state returns a copy of the state.
func (u *Updater) state() *stateFile {
	u.stateMu.Lock()
	defer u.stateMu.Unlock()
	return u.loadLocked().clone()
}

// updateState applies change to a copy of the state and writes it; on an error the state is unchanged.
func (u *Updater) updateState(change func(st *stateFile)) error {
	u.stateMu.Lock()
	defer u.stateMu.Unlock()
	next := u.loadLocked().clone()
	change(next)
	return u.saveLocked(next)
}

// setPending makes rec the pending update; the installer of the previous one, if another, is deleted.
func (u *Updater) setPending(rec *pendingRec) error {
	u.stateMu.Lock()
	defer u.stateMu.Unlock()
	cur := u.loadLocked()
	next := cur.clone()
	next.Pending = rec
	if err := u.saveLocked(next); err != nil {
		return err
	}
	if old := cur.Pending; old != nil && old.File != rec.File {
		u.removeFile(old.File)
	}
	return nil
}

// dropPending clears the pending update and deletes its installer.
func (u *Updater) dropPending(why string) {
	u.stateMu.Lock()
	defer u.stateMu.Unlock()
	cur := u.loadLocked()
	if cur.Pending == nil {
		return
	}
	u.log.Info("pending update discarded", "version", cur.Pending.Version, "why", why)
	u.removeFile(cur.Pending.File)
	next := cur.clone()
	next.Pending = nil
	if err := u.saveLocked(next); err != nil {
		u.log.Warn("can't write the update state", logx.Err(err))
	}
}

// removeFile deletes name from the updates folder; a file that is already gone is fine.
func (u *Updater) removeFile(name string) {
	if err := os.Remove(filepath.Join(u.dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		u.log.Warn("can't delete a file in the updates folder", logx.Err(err))
	}
}

// cleanLocked deletes every regular file of the updates folder but state.json and keep (06 §8.7's cleanup): old
// installers, .partial downloads and temp files of a write that never finished. Call with stateMu.
func (u *Updater) cleanLocked(keep string) {
	entries, err := os.ReadDir(u.dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			u.log.Warn("can't list the updates folder", logx.Err(err))
		}
		return
	}
	for _, e := range entries {
		if name := e.Name(); e.Type().IsRegular() && name != stateName && name != keep {
			u.removeFile(name)
		}
	}
}

// verifyFile checks that path has exactly size bytes and the SHA-256 sum (lowercase hex).
func verifyFile(path string, size int64, sum string) error {
	f, err := os.Open(path) //nolint:gosec // G304: a plain name in the updates folder, from state.json
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, size+1))
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("%s: %d bytes, want %d", filepath.Base(path), n, size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sum {
		return fmt.Errorf("%s: SHA-256 %s, want %s", filepath.Base(path), got, sum)
	}
	return nil
}
