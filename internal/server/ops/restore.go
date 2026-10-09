package ops

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/config"
)

// Restore (04 §12.4). Two inputs are accepted: a backup archive (backup.go), which replaces the database,
// secrets.json and, when the archive has them, the certificates; and a bare SQLite database file, typically 03's
// pre-migration backup backups/pre-<schema>-<ts>.db, which replaces the database alone.
//
// A restore has two halves, with the server's shutdown between them:
//
//  1. DataDir.PrepareRestore, while the server still runs (or, offline, while none does): receive the body into
//     restore/incoming, validate it into restore/new, write backups/pre-restore-<ts>.tar.gz of the current state,
//     and write restore/plan.json in the state "swapping". Until the plan is written nothing of the data
//     directory's own files has changed, and a failure leaves no trace but the log.
//  2. CompleteRestore, once nothing has the files open: move the current files to restore/old-<ts>/ and the staged
//     ones into the data directory, and mark the plan "done". The server calls it at the end of its shutdown, and
//     an offline restore right after step 1.
//
// The plan makes step 2 safe to interrupt: RecoverRestore, which runs at every start before anything reads the
// data directory, completes a plan that is still "swapping", from whatever point the swap had reached. After the
// next successful start, FinishRestore removes restore/old-<ts>/ and the plan.
//
// What is in restore/:
//
//	incoming          the body as received
//	new/              the validated files, under their archive names
//	plan.json         {"ts": "20260929T101500Z", "state": "swapping" | "done", "install": […], "evict": […]}
//	old-<ts>/         what the data directory held before the swap
//	snapshot-*.db     the temporary database snapshot of a backup that is being written

// Names in restore/ and backups/.
const (
	restoreIncomingName = "incoming"
	restoreNewName      = "new"
	restorePlanName     = "plan.json"
	restoreOldPrefix    = "old-"
	restoreSnapshotGlob = "snapshot-*.db"
	preRestorePrefix    = "pre-restore-"
	preRestoreSuffix    = ".tar.gz"
)

// keepPreRestore is how many pre-restore backups stay in backups/ (04 §5.1).
const keepPreRestore = 5

// Limits of a restore's input (04 §12.2, §12.4).
const (
	// RestoreMaxBytes is the largest body POST /v1/restore takes: 2 GiB.
	RestoreMaxBytes = 2 << 30
	// restoreMaxEntries bounds the entries of an archive.
	restoreMaxEntries = 10_000
	// restoreMaxManifestBytes bounds manifest.json: ten thousand hashes are well below it.
	restoreMaxManifestBytes = 8 << 20
	// restoreMaxTrailingBytes bounds what may follow the tar's end marker inside the gzip stream: tar pads its
	// archives with zeros to a multiple of its record size, 10 KiB by default.
	restoreMaxTrailingBytes = 1 << 20
)

// Variables only so that tests can lower the two limits that are too large to reach, and can stop a swap between
// two of its steps.
var (
	// restoreMaxBytes is RestoreMaxBytes as PrepareRestore applies it.
	restoreMaxBytes int64 = RestoreMaxBytes
	// restoreMaxUnpackedBytes bounds what an archive unpacks to, so that a small body can't fill the disk: 16 GiB.
	restoreMaxUnpackedBytes int64 = 16 << 30
	// renameFile moves one file or directory of a swap.
	renameFile = os.Rename
)

// sqliteMagic starts every SQLite database file; gzipMagic every gzip stream.
var (
	sqliteMagic = []byte("SQLite format 3\x00")
	gzipMagic   = []byte{0x1f, 0x8b}
)

// RestoreKind says what a restore's input was.
type RestoreKind string

const (
	// RestoreArchive is a backup archive: the database, secrets.json and maybe the certificates are replaced.
	RestoreArchive RestoreKind = "archive"
	// RestoreDatabase is a bare database file: only the database is replaced.
	RestoreDatabase RestoreKind = "database"
)

// RestoreInfo describes a restore that PrepareRestore has accepted.
type RestoreInfo struct {
	Kind RestoreKind
	// Manifest is the archive's manifest; the zero value for a database file.
	Manifest Manifest
	// SchemaVersion is the schema of the database that takes the place of the current one. An older one is
	// migrated by the start that follows.
	SchemaVersion int
	// PreRestoreBackup is the archive of the state before the restore, relative to the data directory and with
	// forward slashes: backups/pre-restore-20260929T101500Z.tar.gz. "" when the data directory had nothing to back
	// up.
	PreRestoreBackup string
}

// Plan states.
const (
	planSwapping = "swapping" // the staged files are valid and the swap may be anywhere between not begun and complete
	planDone     = "done"     // the swap is complete; restore/old-<ts> waits for the next successful start
)

// restorePlan is restore/plan.json. Install and Evict hold archive names (dataPath says where each lives).
type restorePlan struct {
	// TS names this restore: restore/old-<TS>/ and backups/pre-restore-<TS>.tar.gz.
	TS    string `json:"ts"`
	State string `json:"state"`
	// Install are the staged files in restore/new that replace the data directory's, in the order in which they
	// are moved in: the database before its WAL.
	Install []string `json:"install"`
	// Evict are the files that are only moved away, because they belong to a file that is replaced: the old
	// database's -wal and -shm, which must never sit next to the new database (SQLite would replay the WAL into
	// the wrong file).
	Evict []string `json:"evict"`
}

var planTS = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z$`)

// dataPath is where an archive name lives in the data directory.
func dataPath(p config.Paths, name string) (string, bool) {
	switch name {
	case backupDB:
		return p.DB, true
	case backupWAL:
		return p.DB + "-wal", true
	case backupSHM:
		return p.DB + "-shm", true
	case backupSecrets:
		return p.Secrets, true
	case backupCerts:
		return p.CertMagic, true
	default:
		return "", false
	}
}

// PrepareRestore is the first half of a restore (04 §12.4 steps 1 to 3): it receives body, validates it, writes
// the pre-restore backup of the current state and then the plan. size is the body's length when the caller knows
// it (a Content-Length, a file's size), or -1.
//
// Validation: the body is a gzip'd tar whose first entry is a manifest of a format this build reads; its entries
// are regular files and directories only, named by the manifest's contents, with plain relative names, at most
// 10,000 of them, and every file has the SHA-256 the manifest gives. Or it is one SQLite database file. Either
// way the database is then asked itself, through DB.Inspect, whatever a manifest says: a schema newer than this
// build's is backup_newer; a migration history this build does not know, a failed integrity check, or a file that
// is no isshoni database is backup_invalid. An archive's secrets.json must be one that the server can load.
//
// Its refusals are *DataError values (backup_invalid, backup_newer, restore_in_progress, insufficient_storage,
// payload_too_large). On any error the staging is removed again and the data directory is as it was. After a nil
// error the caller must stop whatever has the data directory's files open and call CompleteRestore; if it never
// does, the next start completes the restore (RecoverRestore).
func (d *DataDir) PrepareRestore(ctx context.Context, body io.Reader, size int64) (_ RestoreInfo, err error) {
	if d.DB.Inspect == nil || d.DB.LatestSchemaVersion == nil {
		return RestoreInfo{}, errors.New("ops: restore: DataDir without DB.Inspect and DB.LatestSchemaVersion")
	}
	p := d.Paths
	plan, err := readPlan(p)
	if err != nil {
		return RestoreInfo{}, err
	}
	if plan != nil && plan.State == planSwapping {
		return RestoreInfo{}, &DataError{
			Code:    api.CodeRestoreInProgress,
			Message: "another restore is in progress: its files are staged and wait for the server's restart",
			Fix:     "let that restore finish (the server completes it when it starts), then decide whether this one is still needed",
		}
	}
	if size > restoreMaxBytes {
		return RestoreInfo{}, tooLarge()
	}
	if err := os.MkdirAll(p.Restore, 0o700); err != nil {
		return RestoreInfo{}, storageError(fmt.Errorf("ops: restore: %w", err))
	}
	if free, ok := freeBytes(p.Restore); ok && size > 0 && uint64(size) > free {
		return RestoreInfo{}, storageError(fmt.Errorf("ops: restore: the backup has %d bytes and %s has %d free: %w",
			size, p.Restore, free, errNoSpace))
	}
	incoming := filepath.Join(p.Restore, restoreIncomingName)
	staged := filepath.Join(p.Restore, restoreNewName)
	clean := func() {
		_ = os.Remove(incoming)
		_ = os.RemoveAll(staged)
	}
	clean() // what an earlier attempt left behind
	defer func() {
		if err != nil {
			clean()
		}
	}()

	if err := receive(ctx, incoming, body); err != nil {
		return RestoreInfo{}, err
	}
	kind, err := sniffKind(incoming)
	if err != nil {
		return RestoreInfo{}, err
	}
	if err := os.Mkdir(staged, 0o700); err != nil {
		return RestoreInfo{}, storageError(fmt.Errorf("ops: restore: %w", err))
	}
	info := RestoreInfo{Kind: kind}
	next := restorePlan{State: planSwapping, Install: []string{backupDB}}
	switch kind {
	case RestoreDatabase:
		if err := os.Rename(incoming, filepath.Join(staged, backupDB)); err != nil {
			return RestoreInfo{}, fmt.Errorf("ops: restore: %w", err)
		}
	default:
		if info.Manifest, err = extractArchive(ctx, incoming, staged); err != nil {
			return RestoreInfo{}, err
		}
		_ = os.Remove(incoming)
		contents := info.Manifest.Contents
		if info.Manifest.Raw && slices.Contains(contents, backupWAL) {
			next.Install = append(next.Install, backupWAL)
		}
		next.Install = append(next.Install, backupSecrets)
		if slices.Contains(contents, backupCerts+"/") {
			next.Install = append(next.Install, backupCerts)
		}
		if err := config.InspectSecrets(filepath.Join(staged, backupSecrets)); err != nil {
			return RestoreInfo{}, invalidBackup("its secrets.json is not one the server can load", err)
		}
	}
	// The old database's WAL and its index never survive next to another database; a staged -shm is not installed
	// either, because SQLite builds a new one.
	for _, name := range []string{backupWAL, backupSHM} {
		if !slices.Contains(next.Install, name) {
			next.Evict = append(next.Evict, name)
		}
	}
	if info.SchemaVersion, err = d.checkDatabase(ctx, filepath.Join(staged, backupDB)); err != nil {
		return RestoreInfo{}, err
	}
	if err := syncDir(staged); err != nil {
		return RestoreInfo{}, err
	}

	// Step 2: the current state, so that this restore can itself be undone.
	next.TS = d.freeTS()
	if info.PreRestoreBackup, err = d.preRestoreBackup(ctx, next.TS); err != nil {
		return RestoreInfo{}, err
	}
	// Step 3: from here on the restore will happen, in this process or at the next start. A restore that was done
	// but never followed by a successful start (two offline restores in a row) makes room first, so that there is
	// one plan and one old-<ts> at a time: its old files go, and its pre-restore backup stays.
	err = FinishRestore(p, d.Logger)
	if err == nil {
		err = writePlan(p, &next)
	}
	if err != nil {
		if info.PreRestoreBackup != "" {
			_ = os.Remove(filepath.Join(p.DataDir, filepath.FromSlash(info.PreRestoreBackup)))
		}
		return RestoreInfo{}, storageError(err)
	}
	return info, nil
}

// tooLarge is the refusal of a body over RestoreMaxBytes.
func tooLarge() *DataError {
	return &DataError{
		Code:    api.CodePayloadTooLarge,
		Message: "the backup is larger than 2 GiB, the most a restore takes",
	}
}

// receive writes body to a new file at path, at most RestoreMaxBytes of it, and syncs it.
func receive(ctx context.Context, path string, body io.Reader) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: restore/incoming
	if err != nil {
		return storageError(fmt.Errorf("ops: restore: %w", err))
	}
	defer func() {
		if cerr := f.Close(); err == nil && cerr != nil {
			err = storageError(fmt.Errorf("ops: restore: %w", cerr))
		}
	}()
	n, err := copyContext(ctx, f, io.LimitReader(body, restoreMaxBytes+1))
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig), err == nil && n > restoreMaxBytes:
		return tooLarge()
	case err != nil:
		return storageError(fmt.Errorf("ops: restore: receiving the backup: %w", err))
	}
	if err := f.Sync(); err != nil {
		return storageError(fmt.Errorf("ops: restore: %w", err))
	}
	return nil
}

// sniffKind tells an archive from a database file by the first bytes: what a client says the body is counts for
// nothing.
func sniffKind(path string) (RestoreKind, error) {
	f, err := os.Open(path) //nolint:gosec // G304: restore/incoming
	if err != nil {
		return "", fmt.Errorf("ops: restore: %w", err)
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, len(sqliteMagic))
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", fmt.Errorf("ops: restore: %w", err)
	}
	return KindOf(head[:n])
}

// KindOf tells what a restore's input is from its first bytes (16 are enough): a backup archive, a database file,
// or neither, which is a *DataError with backup_invalid. `isshoni admin restore` asks it before it sends a file.
func KindOf(head []byte) (RestoreKind, error) {
	switch {
	case len(head) >= len(sqliteMagic) && string(head[:len(sqliteMagic)]) == string(sqliteMagic):
		return RestoreDatabase, nil
	case len(head) >= len(gzipMagic) && string(head[:len(gzipMagic)]) == string(gzipMagic):
		return RestoreArchive, nil
	case len(head) == 0:
		return "", invalidBackup("it is empty", nil)
	default:
		return "", invalidBackup("it is neither a backup archive (.tar.gz) nor an SQLite database file", nil)
	}
}

// checkDatabase asks the staged database what it is (04 §12.4 step 1) and returns its schema version.
func (d *DataDir) checkDatabase(ctx context.Context, path string) (int, error) {
	info, err := d.DB.Inspect(ctx, path)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, ctxErr
		}
		return 0, invalidBackup("its database can't be read as an SQLite database", err)
	}
	switch {
	case info.SchemaVersion > d.DB.LatestSchemaVersion():
		return 0, newerBackup(info.LastAppVersion)
	case info.Integrity != nil:
		return 0, invalidBackup("its database is damaged (the integrity check failed)", info.Integrity)
	case !info.HistoryOK:
		return 0, invalidBackup("its database has a migration history that this isshoni does not know; another build wrote it", nil)
	case info.SchemaVersion < 1:
		return 0, invalidBackup("its database is not an isshoni database", nil)
	}
	return info.SchemaVersion, nil
}

// extractArchive validates the archive at src and unpacks it into dir, through an os.Root: whatever a name in the
// archive says, nothing is written outside dir.
func extractArchive(ctx context.Context, src, dir string) (Manifest, error) {
	f, err := os.Open(src) //nolint:gosec // G304: restore/incoming
	if err != nil {
		return Manifest{}, fmt.Errorf("ops: restore: %w", err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return Manifest{}, invalidBackup("it is not a gzip file", err)
	}
	defer func() { _ = gz.Close() }()
	gz.Multistream(false)
	tr := tar.NewReader(gz)
	m, err := readManifest(tr)
	if err != nil {
		return Manifest{}, err
	}
	files, certs, err := manifestContents(m)
	if err != nil {
		return Manifest{}, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return Manifest{}, fmt.Errorf("ops: restore: %w", err)
	}
	defer func() { _ = root.Close() }()

	seen := map[string]bool{}
	entries := 1 // the manifest
	var unpacked int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return Manifest{}, ctxErr
			}
			return Manifest{}, invalidBackup("it is damaged or cut off", err)
		}
		if entries++; entries > restoreMaxEntries {
			return Manifest{}, invalidBackup(fmt.Sprintf("it has more than %d entries", restoreMaxEntries), nil)
		}
		name, isDir, err := entryName(hdr)
		if err != nil {
			return Manifest{}, err
		}
		inCerts := name == backupCerts || strings.HasPrefix(name, backupCerts+"/")
		switch {
		case seen[name]:
			return Manifest{}, invalidBackup("it holds "+quoteName(name)+" twice", nil)
		case inCerts && !certs, !inCerts && (isDir || !slices.Contains(files, name)), name == backupCerts && !isDir:
			return Manifest{}, invalidBackup("it holds "+quoteName(name)+", which its manifest does not list", nil)
		}
		seen[name] = true
		local := filepath.FromSlash(name)
		if isDir {
			if err := root.MkdirAll(local, 0o700); err != nil {
				return Manifest{}, storageError(fmt.Errorf("ops: restore: unpacking %s: %w", name, err))
			}
			continue
		}
		if unpacked += hdr.Size; hdr.Size < 0 || unpacked > restoreMaxUnpackedBytes {
			return Manifest{}, invalidBackup("it unpacks to more than 16 GiB", nil)
		}
		if free, ok := freeBytes(dir); ok && uint64(hdr.Size) > free {
			return Manifest{}, storageError(fmt.Errorf("ops: restore: %s has %d bytes and %s has %d free: %w", name, hdr.Size, dir, free, errNoSpace))
		}
		want, ok := m.SHA256[name]
		if !ok {
			return Manifest{}, invalidBackup("its manifest has no checksum for "+quoteName(name), nil)
		}
		got, err := unpackFile(ctx, root, local, tr)
		switch {
		case err == nil:
		case ctx.Err() != nil:
			return Manifest{}, ctx.Err()
		case errors.Is(err, errUnpackWrite): // the disk, not the archive
			return Manifest{}, storageError(fmt.Errorf("ops: restore: unpacking %s: %w", name, err))
		default:
			return Manifest{}, invalidBackup("it is damaged or cut off", err)
		}
		if !strings.EqualFold(want, got) {
			return Manifest{}, invalidBackup("the checksum of "+quoteName(name)+" does not match its manifest: the archive is damaged", nil)
		}
	}
	// The gzip trailer carries the stream's own checksum; it is only looked at when the stream is read to its end.
	// What follows the tar's end is its padding, a few kilobytes of zeros; an archive with more than that is none
	// of ours, and reading on would only burn time.
	rest, err := io.Copy(io.Discard, io.LimitReader(gz, restoreMaxTrailingBytes+1))
	switch {
	case err != nil:
		return Manifest{}, invalidBackup("it is damaged or cut off", err)
	case rest > restoreMaxTrailingBytes:
		return Manifest{}, invalidBackup("it has data after the archive's end", nil)
	}
	for _, name := range files {
		if !seen[name] {
			return Manifest{}, invalidBackup("it lacks "+quoteName(name)+", which its manifest lists", nil)
		}
	}
	for name := range m.SHA256 {
		if !seen[name] {
			return Manifest{}, invalidBackup("it lacks "+quoteName(name)+", which its manifest has a checksum for", nil)
		}
	}
	if certs {
		if err := root.MkdirAll(backupCerts, 0o700); err != nil {
			return Manifest{}, storageError(fmt.Errorf("ops: restore: %w", err))
		}
	}
	return m, nil
}

// manifestContents checks what a manifest says the archive holds and returns the top-level files and whether the
// certificate storage is among them. A restore needs the database and secrets.json; the -wal and -shm files belong
// to a raw backup only.
func manifestContents(m Manifest) (files []string, certs bool, err error) {
	for _, name := range m.Contents {
		switch {
		case slices.Contains(files, name) || (name == backupCerts+"/" && certs):
			return nil, false, invalidBackup("its manifest lists "+quoteName(name)+" twice", nil)
		case name == backupCerts+"/":
			certs = true
		case name == backupDB, name == backupSecrets, m.Raw && (name == backupWAL || name == backupSHM):
			files = append(files, name)
		default:
			return nil, false, invalidBackup("its manifest lists "+quoteName(name)+", which this isshoni does not know", nil)
		}
	}
	for _, need := range []string{backupDB, backupSecrets} {
		if !slices.Contains(files, need) {
			return nil, false, invalidBackup("it holds no "+need, nil)
		}
	}
	return files, certs, nil
}

// entryName checks one entry's type and name and returns the name without a directory's trailing slash. Only
// regular files and directories pass (no symlink, hard link or device), and only names that are plain relative
// paths in their shortest form: no "..", no "//", no leading "/", no backslash.
func entryName(hdr *tar.Header) (name string, isDir bool, err error) {
	switch hdr.Typeflag {
	case tar.TypeReg:
	case tar.TypeDir:
		isDir = true
	default:
		return "", false, invalidBackup("its entry "+quoteName(hdr.Name)+" is neither a regular file nor a directory", nil)
	}
	// A sparse file says how large it is and holds only a part of that; unpacking writes the rest as zeros, so a
	// few bytes of archive could ask for gigabytes of disk. isshoni writes none.
	for key := range hdr.PAXRecords {
		if strings.HasPrefix(key, "GNU.sparse.") {
			return "", false, invalidBackup("its entry "+quoteName(hdr.Name)+" is a sparse file", nil)
		}
	}
	name = hdr.Name
	if isDir {
		name = strings.TrimSuffix(name, "/")
	}
	if name == "" || name == backupManifest || strings.ContainsAny(name, "\\\x00") || path.IsAbs(name) ||
		path.Clean(name) != name || !filepath.IsLocal(filepath.FromSlash(name)) {
		return "", false, invalidBackup("its entry "+quoteName(hdr.Name)+" has a name that no backup holds", nil)
	}
	return name, isDir, nil
}

// errUnpackWrite marks an unpackFile error that is the destination's, not the archive's.
var errUnpackWrite = errors.New("writing the unpacked file")

// unpackFile writes the current entry of tr to a new file name under root and returns the hex SHA-256 of what it
// wrote. The file is synced: the swap that follows must not move a file whose content is still in memory only.
func unpackFile(ctx context.Context, root *os.Root, name string, tr *tar.Reader) (sum string, err error) {
	if dir := filepath.Dir(name); dir != "." {
		if err := root.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("%w: %w", errUnpackWrite, err)
		}
	}
	out, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errUnpackWrite, err)
	}
	defer func() {
		if cerr := out.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("%w: %w", errUnpackWrite, cerr)
		}
	}()
	h := sha256.New()
	if _, err := copyContext(ctx, &unpackWriter{w: io.MultiWriter(out, h)}, tr); err != nil {
		return "", err
	}
	if err := out.Sync(); err != nil {
		return "", fmt.Errorf("%w: %w", errUnpackWrite, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// unpackWriter marks its write errors as the destination's (errUnpackWrite), so that a full disk is not taken for a
// damaged archive.
type unpackWriter struct{ w io.Writer }

func (u *unpackWriter) Write(p []byte) (int, error) {
	n, err := u.w.Write(p)
	if err != nil {
		err = fmt.Errorf("%w: %w", errUnpackWrite, err)
	}
	return n, err
}

// freeTS returns the timestamp that names this restore: now, to the second, moved on while a pre-restore backup or
// an old-<ts> directory of that second exists.
func (d *DataDir) freeTS() string {
	at := d.now().UTC()
	for {
		ts := at.Format(backupTimeFormat)
		_, errBackup := os.Lstat(filepath.Join(d.Paths.Backups, preRestorePrefix+ts+preRestoreSuffix))
		_, errOld := os.Lstat(filepath.Join(d.Paths.Restore, restoreOldPrefix+ts))
		if errors.Is(errBackup, fs.ErrNotExist) && errors.Is(errOld, fs.ErrNotExist) {
			return ts
		}
		at = at.Add(time.Second)
	}
}

// preRestoreBackup writes backups/pre-restore-<ts>.tar.gz, a full backup of the current state (04 §12.4 step 2),
// and keeps the newest five. It returns the file's path relative to the data directory, or "" when the data
// directory has nothing to back up: no database, no secrets.json and no certificates.
func (d *DataDir) preRestoreBackup(ctx context.Context, ts string) (string, error) {
	b, err := d.PrepareBackup(ctx, BackupOptions{})
	if err != nil {
		return "", fmt.Errorf("ops: restore: backup of the current state: %w", err)
	}
	defer b.Close()
	if b.Empty() {
		return "", nil
	}
	if err := os.MkdirAll(d.Paths.Backups, 0o700); err != nil {
		return "", storageError(fmt.Errorf("ops: restore: %w", err))
	}
	name := preRestorePrefix + ts + preRestoreSuffix
	if err := b.WriteBackupFile(ctx, filepath.Join(d.Paths.Backups, name)); err != nil {
		return "", fmt.Errorf("ops: restore: backup of the current state: %w", err)
	}
	if err := rotatePreRestore(d.Paths.Backups); err != nil {
		d.log().Warn("could not remove old pre-restore backups", logx.Err(err))
	}
	return path.Join(filepath.Base(d.Paths.Backups), name), nil
}

// rotatePreRestore deletes the pre-restore backups in dir beyond the newest keepPreRestore, by the timestamp in
// their names. 03's pre-migration backups (pre-<schema>-<ts>.db) are not its business.
func rotatePreRestore(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("ops: restore: %w", err)
	}
	var names []string
	for _, e := range entries {
		ts, hasPrefix := strings.CutPrefix(e.Name(), preRestorePrefix)
		ts, hasSuffix := strings.CutSuffix(ts, preRestoreSuffix)
		if hasPrefix && hasSuffix && planTS.MatchString(ts) && e.Type().IsRegular() {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	var errs []error
	for i := 0; i < len(names)-keepPreRestore; i++ {
		if err := os.Remove(filepath.Join(dir, names[i])); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// readPlan reads restore/plan.json; it returns nil without an error when there is none.
func readPlan(p config.Paths) (*restorePlan, error) {
	file := filepath.Join(p.Restore, restorePlanName)
	data, err := os.ReadFile(file) //nolint:gosec // G304: restore/plan.json
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ops: restore: %w", err)
	}
	var plan restorePlan
	unusable := func(why string) error {
		return fmt.Errorf("%s is the record of a restore, and it can't be used (%s). Compare the data directory with %s, "+
			"put the files where they belong, then remove %s", file, why, p.Restore, file)
	}
	if err := json.Unmarshal(data, &plan); err != nil {
		return nil, unusable("it is not valid JSON")
	}
	if !planTS.MatchString(plan.TS) || (plan.State != planSwapping && plan.State != planDone) {
		return nil, unusable("its ts or its state is not one a restore writes")
	}
	for _, name := range slices.Concat(plan.Install, plan.Evict) {
		if _, ok := dataPath(p, name); !ok {
			return nil, unusable("it names " + quoteName(name) + ", which is no file of the data directory")
		}
	}
	return &plan, nil
}

// writePlan writes restore/plan.json atomically: a new file, synced, renamed over the old one, and the directory
// synced. A crash leaves the old plan or the new one, never a part of one.
func writePlan(p config.Paths, plan *restorePlan) error {
	data, err := json.Marshal(plan)
	if err != nil {
		return fmt.Errorf("ops: restore: plan: %w", err)
	}
	file := filepath.Join(p.Restore, restorePlanName)
	tmp := file + ".tmp-" + randomName()
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: restore/plan.json.tmp-*
	if err != nil {
		return fmt.Errorf("ops: restore: plan: %w", err)
	}
	_, err = f.Write(append(data, '\n'))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, file)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("ops: restore: plan: %w", err)
	}
	return syncDir(p.Restore)
}

// CompleteRestore is the second half of a restore (04 §12.4 steps 5 and 6): when restore/plan.json is in the state
// "swapping", it moves the data directory's current files to restore/old-<ts>/ and the staged ones in, marks the
// plan "done" and reports true. Without such a plan it does nothing. Nothing may have the data directory's files
// open: the server calls it once its store is closed, and the offline restore under the data-directory lock.
//
// It may be interrupted anywhere and called again: every step looks at what is there. The order keeps a WAL from
// ever sitting next to a database it does not belong to: the old -wal and -shm leave before the old database,
// and the new database arrives before a WAL of its own (a raw backup's).
func CompleteRestore(p config.Paths, log *slog.Logger) (bool, error) {
	plan, err := readPlan(p)
	if err != nil || plan == nil || plan.State != planSwapping {
		return false, err
	}
	old := filepath.Join(p.Restore, restoreOldPrefix+plan.TS)
	staged := filepath.Join(p.Restore, restoreNewName)
	if err := os.MkdirAll(old, 0o700); err != nil {
		return false, fmt.Errorf("ops: restore: %w", err)
	}
	// Out: what the data directory holds now. A file that the plan installs and that is no longer staged has been
	// installed by an earlier run, so the one in the data directory is the new one and stays.
	for _, name := range slices.Concat(plan.Evict, plan.Install) {
		target, _ := dataPath(p, name) // readPlan has checked the names
		if slices.Contains(plan.Install, name) && !exists(filepath.Join(staged, name)) {
			continue
		}
		if !exists(target) {
			continue
		}
		kept := filepath.Join(old, name)
		if exists(kept) {
			return false, fmt.Errorf("ops: restore: both %s and %s exist; a restore moves the first to the second "+
				"once. Check which one is the data directory's, move the other away, and start again", target, kept)
		}
		if err := renameFile(target, kept); err != nil {
			return false, fmt.Errorf("ops: restore: %w", err)
		}
	}
	if err := errors.Join(syncDir(old), syncDir(p.DataDir)); err != nil {
		return false, err
	}
	// In: the staged files, in the plan's order.
	for _, name := range plan.Install {
		from := filepath.Join(staged, name)
		if !exists(from) {
			continue
		}
		target, _ := dataPath(p, name)
		if err := renameFile(from, target); err != nil {
			return false, fmt.Errorf("ops: restore: %w", err)
		}
	}
	if err := syncDir(p.DataDir); err != nil {
		return false, err
	}
	plan.State = planDone
	if err := writePlan(p, plan); err != nil {
		return false, err
	}
	_ = os.RemoveAll(staged)
	_ = os.Remove(filepath.Join(p.Restore, restoreIncomingName))
	dataLogger(log).Info("restore: the data directory holds the restored files; the previous ones are kept until the next successful start",
		slog.String("previous", old))
	return true, nil
}

// RecoverRestore puts restore/ in order before anything reads the data directory (04 §12.4 "Crash safety"): the
// server calls it at startup right after it has the data-directory lock, and the offline commands do the same. A
// plan that is still "swapping" is completed, from wherever its swap stopped. Staging that no plan claims (the
// body, the unpacked files, a backup's snapshot) is left over from a restore or a backup that never got as far as
// its plan, and is removed with a log line.
//
// Its error means that the data directory may be half swapped and the server must not open it: a plan that can't
// be read, or a swap that fails again.
func RecoverRestore(p config.Paths, log *slog.Logger) error {
	log = dataLogger(log)
	if !exists(p.Restore) {
		return nil
	}
	plan, err := readPlan(p)
	if err != nil {
		return err
	}
	if plan != nil && plan.State == planSwapping {
		log.Warn("a restore was interrupted before its files were swapped in; completing it now")
		if _, err := CompleteRestore(p, log); err != nil {
			return fmt.Errorf("completing the interrupted restore: %w", err)
		}
	}
	var leftovers []string
	for _, name := range []string{restoreIncomingName, restoreNewName} {
		if exists(filepath.Join(p.Restore, name)) {
			leftovers = append(leftovers, name)
		}
	}
	for _, pattern := range []string{restoreSnapshotGlob, restorePlanName + ".tmp-*"} {
		matches, _ := filepath.Glob(filepath.Join(p.Restore, pattern)) // the pattern is ours and well-formed
		for _, m := range matches {
			leftovers = append(leftovers, filepath.Base(m))
		}
	}
	var errs []error
	for _, name := range leftovers {
		if err := os.RemoveAll(filepath.Join(p.Restore, name)); err != nil {
			errs = append(errs, err)
		}
	}
	if len(leftovers) > 0 {
		log.Info("removed what an unfinished restore or backup left in the staging directory",
			slog.String("dir", p.Restore), slog.Any("removed", leftovers))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("ops: restore: cleaning %s: %w", p.Restore, err)
	}
	return nil
}

// FinishRestore is the last step of a restore (04 §12.4 step 7), for after the next successful start: when the
// plan is "done", it removes restore/old-<ts>/, the data directory's files from before the restore, and then the
// plan. The pre-restore backup in backups/ stays. Without a done plan it does nothing.
func FinishRestore(p config.Paths, log *slog.Logger) error {
	plan, err := readPlan(p)
	if err != nil || plan == nil || plan.State != planDone {
		return err
	}
	old := filepath.Join(p.Restore, restoreOldPrefix+plan.TS)
	if err := os.RemoveAll(old); err != nil {
		return fmt.Errorf("ops: restore: %w", err)
	}
	if err := os.Remove(filepath.Join(p.Restore, restorePlanName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("ops: restore: %w", err)
	}
	dataLogger(log).Info("restore finished: removed the files from before the restore", slog.String("removed", old))
	return syncDir(p.Restore)
}

// exists reports whether path exists (any type).
func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
