package ops

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/ops/doctor"
	"github.com/MoonWX/isshoni/internal/version"
)

// Backup archives (04 §12.3). A backup is one .tar.gz with everything that makes a server this server: a consistent
// snapshot of the database, secrets.json, and the certificate storage, so that a restore neither signs everyone
// out nor orders certificates again.
//
//	isshoni-backup-0.3.0-20260929T101500Z.tar.gz
//	├─ manifest.json
//	├─ isshoni.db          VACUUM INTO a temporary file: 03's (*store.DB).BackupTo, or store.BackupFile offline
//	├─ secrets.json
//	└─ certmagic/…         the certificate storage without locks/ (left out with --no-certs)
//
// The manifest comes first, so a reader knows what follows before it reads it. That needs every size and hash up
// front, hence the two steps: DataDir.PrepareBackup stages the archive's content, Backup.WriteTo streams it. The
// database snapshot is a temporary file in restore/ (the data directory's staging place, 04 §5.1); secrets.json and
// the certificate files are small and are read into memory when the backup is prepared, so that the bytes in the
// archive are the ones that were hashed even when a renewal rewrites a certificate meanwhile.
//
// `isshoni admin backup` reaches this through the admin socket's GET /v1/backup, and `--offline` calls it directly
// on the data directory of a stopped server (04 §12.6); a restore writes its pre-restore backup with it (restore.go).

// BackupFormat is the "format" of the manifests this build writes, and the newest it reads.
const BackupFormat = 1

// The names of an archive's entries. They are fixed by the format; where each lives in the data directory is
// config.Paths' business (dataPath).
const (
	backupManifest = "manifest.json"
	backupDB       = "isshoni.db"
	backupWAL      = "isshoni.db-wal" //nolint:gosec // G101: a file name. Raw backups only
	backupSHM      = "isshoni.db-shm" // raw backups only
	backupSecrets  = "secrets.json"
	backupCerts    = "certmagic" // a directory: "certmagic/" in the manifest's contents
)

// certLocksDir is the part of the certificate storage that no backup holds: certmagic's lock files, which mean
// something only to the process that wrote them.
const certLocksDir = "locks"

// Bounds on what a backup reads into memory. A certificate, its key and its metadata are a few kilobytes each, and
// a server has one certificate and an ACME account or two.
const (
	backupMaxCertFile  = 16 << 20
	backupMaxCertTotal = 256 << 20
	backupMaxSecrets   = 1 << 20
)

// backupTimeFormat is the timestamp in the names of backup archives, pre-restore backups and restore/old-<ts>: UTC,
// as in 03's pre-migration backups.
const backupTimeFormat = "20060102T150405Z"

// Manifest is manifest.json, the first entry of a backup archive (04 §12.3).
type Manifest struct {
	Format         int    `json:"format"`         // BackupFormat
	IsshoniVersion string `json:"isshoniVersion"` // the build that wrote the archive, e.g. "0.3.0"
	// SchemaVersion is the schema of the database in the archive; 0 when it could not be read (a raw backup of a
	// damaged file). A restore never trusts it: it asks the database itself (04 §12.4).
	SchemaVersion int          `json:"schemaVersion"`
	CreatedAt     api.WireTime `json:"createdAt"`
	// Contents names what the archive holds, in archive order: "isshoni.db", "secrets.json", and "certmagic/" for
	// the directory. An archive made of a data directory that lacked one of them lacks it too.
	Contents []string `json:"contents"`
	// SHA256 has the hex SHA-256 of every file in the archive by its name, the files below certmagic/ included.
	SHA256 map[string]string `json:"sha256"`
	// Raw says that the database is not a snapshot but a copy of the files as they were: isshoni.db with its -wal
	// and -shm. An offline backup does that only when the snapshot fails, for example on a corrupt database.
	Raw bool `json:"raw,omitempty"`
}

// BackupFileName is the name of a backup archive written by the build ver at the time at:
// isshoni-backup-0.3.0-20260929T101500Z.tar.gz.
func BackupFileName(ver string, at time.Time) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', strings.ContainsRune(".-+_", r):
			return r
		default:
			return '_'
		}
	}, ver)
	return "isshoni-backup-" + safe + "-" + at.UTC().Format(backupTimeFormat) + ".tar.gz"
}

// DataError is a refusal of a backup or a restore that the operator can act on: the admin socket answers it with
// Code and the English text (04 §12.2), and the offline commands print the same text. It unwraps to an *api.Error
// with the code, so api.IsCode works on it. Any other error of this file and of restore.go is unexpected: a file
// that can't be read or written.
type DataError struct {
	Code    string // api.CodeBackupInvalid, CodeBackupNewer, CodeRestoreInProgress, CodeInsufficientStorage, CodePayloadTooLarge
	Message string // English, for the operator
	Fix     string // English; may be ""
	Err     error  // the cause, or nil
}

func (e *DataError) Error() string { return e.Message }

// Unwrap returns the *api.Error of the code and the cause.
func (e *DataError) Unwrap() []error {
	out := []error{api.NewError(e.Code)}
	if e.Err != nil {
		out = append(out, e.Err)
	}
	return out
}

// DataDir is the data directory as backup and restore act on it (04 §12.3, §12.4): the same code serves the running
// server, behind the admin socket, and the offline commands of a stopped one (04 §12.6). ops may not import store
// (04 §2), so the wiring and cmd/isshoni fill in 03's functions.
type DataDir struct {
	// Paths is the layout of the data directory (config.Config.Paths).
	Paths config.Paths
	// DB carries 03's file-level functions: Inspect, which a restore validates every database with, and Backup
	// (store.BackupFile), the snapshot of an offline backup. Inspect is required.
	DB doctor.DBFiles
	// Snapshot writes a consistent copy of the running database to dst, which does not exist yet:
	// (*store.DB).BackupTo. nil means that no server has the database open: the snapshot is then DB.Backup on the
	// file, and a copy of the raw files when that fails.
	Snapshot func(ctx context.Context, dst string) error
	// Flush, when set, runs before a snapshot of the running database, so that what the server still holds in
	// memory is in it: ops.Transfer.Flush once the transfer counters are wired (04 §11.3; README S85). Its error is
	// logged and the backup goes on.
	Flush func(ctx context.Context) error
	// Now is the clock for the timestamps in manifests and file names. nil means time.Now.
	Now func() time.Time
	// Logger gets the lines of 04 §12.4 (a restore completed at startup, staging removed, …). nil means
	// slog.Default().
	Logger *slog.Logger
}

func (d *DataDir) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *DataDir) log() *slog.Logger { return dataLogger(d.Logger) }

// dataLogger returns log (slog.Default() when nil) with the component of backup and restore.
func dataLogger(log *slog.Logger) *slog.Logger {
	if log == nil {
		log = slog.Default()
	}
	return log.With(slog.String("component", "backup"))
}

// BackupOptions says what a backup holds.
type BackupOptions struct {
	// NoCerts leaves the certificate storage out (`isshoni admin backup --no-certs`). The server then orders new
	// certificates after a restore on another machine, which counts against the CA's rate limits.
	NoCerts bool
}

// Backup is the staged content of one archive: PrepareBackup makes it, WriteTo streams it, Close removes its
// temporary snapshot. Manifest is what the archive will say about itself.
type Backup struct {
	Manifest Manifest

	entries []backupEntry
	temp    []string // files to remove with Close
}

// backupEntry is one entry of the archive after the manifest.
type backupEntry struct {
	name    string // the name in the archive, without a trailing slash
	dir     bool
	modTime time.Time
	size    int64
	data    []byte // a small file, as it was when the backup was prepared
	path    string // or a file on disk that does not change meanwhile: the snapshot, or a raw file of a stopped server
	sum     string // the hex SHA-256 of a top-level file
}

// Filename is the name the archive should get: isshoni-backup-<version>-<created>.tar.gz.
func (b *Backup) Filename() string {
	return BackupFileName(b.Manifest.IsshoniVersion, time.Time(b.Manifest.CreatedAt))
}

// Empty reports whether the data directory had nothing to back up: no database, no secrets.json and no
// certificates. Such a backup is still a valid archive of nothing; a restore's pre-restore backup skips it.
func (b *Backup) Empty() bool { return len(b.entries) == 0 }

// Close removes the backup's temporary files. Call it when the archive is written, or given up.
func (b *Backup) Close() {
	for _, name := range b.temp {
		_ = os.Remove(name)
	}
	b.temp = nil
}

// PrepareBackup stages a backup of the data directory: it snapshots the database into a temporary file, reads
// secrets.json and, unless o.NoCerts, the certificate storage, and computes the manifest. Nothing in the data
// directory is changed. The caller writes the archive with WriteTo and then calls Close.
//
// With Snapshot set (the running server) a snapshot that fails is the error. Without it (offline), the snapshot of
// the database file is DB.Backup, and only when that fails are the raw files copied instead, with "raw": true in the
// manifest (04 §12.3). A disk without room for the snapshot is neither: it is the refusal insufficient_storage, with
// or without a server, and it is seen before the snapshot begins. A part that the data directory does not have is
// left out of the archive.
func (d *DataDir) PrepareBackup(ctx context.Context, o BackupOptions) (_ *Backup, err error) {
	b := &Backup{Manifest: Manifest{
		Format:         BackupFormat,
		IsshoniVersion: version.Version(),
		CreatedAt:      api.WireTime(d.now().UTC().Truncate(time.Second)),
		Contents:       []string{},
		SHA256:         map[string]string{},
	}}
	defer func() {
		if err != nil {
			b.Close()
		}
	}()
	if err := d.stageDatabase(ctx, b); err != nil {
		return nil, err
	}
	if err := d.stageSecrets(b); err != nil {
		return nil, err
	}
	if !o.NoCerts {
		if err := d.stageCerts(ctx, b); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// stageDatabase adds the database to b: a snapshot, or the raw files of a stopped server whose database can't be
// read.
func (d *DataDir) stageDatabase(ctx context.Context, b *Backup) error {
	online := d.Snapshot != nil
	if !online {
		if _, err := os.Lstat(d.Paths.DB); errors.Is(err, fs.ErrNotExist) {
			return nil // a data directory without a database: nothing to snapshot
		}
	}
	if err := os.MkdirAll(d.Paths.Restore, 0o700); err != nil {
		return storageError(fmt.Errorf("ops: backup: %w", err))
	}
	if err := d.snapshotRoom(); err != nil {
		return storageError(fmt.Errorf("ops: backup: snapshot of the database: %w", err))
	}
	tmp := filepath.Join(d.Paths.Restore, "snapshot-"+randomName()+".db")
	var err error
	switch {
	case online:
		if d.Flush != nil {
			if ferr := d.Flush(ctx); ferr != nil {
				d.log().Warn("could not flush the server's counters before the backup; it holds them as last stored", logx.Err(ferr))
			}
		}
		err = d.Snapshot(ctx, tmp)
	case d.DB.Backup == nil:
		err = errors.New("ops: backup: DataDir has neither Snapshot nor DB.Backup")
	default:
		err = d.DB.Backup(ctx, d.Paths.DB, tmp)
	}
	if err != nil {
		_ = os.Remove(tmp)
		// A full disk says nothing about the database: the raw files are for one that can't be read.
		if ctx.Err() != nil || online || d.DB.Backup == nil || noSpace(err) {
			return storageError(fmt.Errorf("ops: backup: snapshot of the database: %w", err))
		}
		d.log().Warn("the database can't be read as a database; the backup holds its raw files instead",
			slog.String("path", d.Paths.DB), logx.Err(err))
		return d.stageRawDatabase(ctx, b)
	}
	b.temp = append(b.temp, tmp)
	entry, err := fileEntry(ctx, backupDB, tmp)
	if err != nil {
		return err
	}
	b.add(entry)
	b.Manifest.SchemaVersion = d.schemaOf(ctx, tmp)
	return nil
}

// snapshotRoom looks ahead at the snapshot of the database, which is a second copy of it on the data directory's
// disk: without the room for it the error says so (errNoSpace), before a server fills its own disk to find out, and
// before an offline backup takes the failed snapshot for a database that can't be read. The database's size with
// its WAL is the most a snapshot takes; it is smaller when the database has pages that it no longer uses. Where the
// platform can't tell the free space, the snapshot's own error has to do (the wiring marks SQLite's "disk is full").
func (d *DataDir) snapshotRoom() error {
	var need uint64
	for _, file := range []string{d.Paths.DB, d.Paths.DB + "-wal"} {
		fi, err := os.Stat(file)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if size := fi.Size(); size > 0 {
			need += uint64(size)
		}
	}
	free, ok := freeBytes(d.Paths.Restore)
	if !ok || need <= free {
		return nil
	}
	return fmt.Errorf("the database has %d bytes and %s has %d free: %w", need, d.Paths.Restore, free, errNoSpace)
}

// stageRawDatabase adds the database's files as they are (04 §12.3 "raw"). Nothing has them open: the caller is an
// offline command, which holds the data-directory lock.
func (d *DataDir) stageRawDatabase(ctx context.Context, b *Backup) error {
	b.Manifest.Raw = true
	for _, f := range []struct{ name, path string }{
		{backupDB, d.Paths.DB}, {backupWAL, d.Paths.DB + "-wal"}, {backupSHM, d.Paths.DB + "-shm"},
	} {
		fi, err := os.Lstat(f.path)
		if errors.Is(err, fs.ErrNotExist) || (err == nil && !fi.Mode().IsRegular()) {
			continue
		}
		if err != nil {
			return fmt.Errorf("ops: backup: %w", err)
		}
		entry, err := fileEntry(ctx, f.name, f.path)
		if err != nil {
			return err
		}
		b.add(entry)
	}
	return nil
}

// schemaOf is the schema version of the database file at path, for the manifest; 0 when it can't be read.
func (d *DataDir) schemaOf(ctx context.Context, path string) int {
	if d.DB.Inspect == nil {
		return 0
	}
	info, err := d.DB.Inspect(ctx, path)
	if err != nil {
		return 0
	}
	return info.SchemaVersion
}

// stageSecrets adds secrets.json as it is on disk. A corrupt file is backed up too: the copy is what is left of it.
func (d *DataDir) stageSecrets(b *Backup) error {
	fi, err := os.Lstat(d.Paths.Secrets)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("ops: backup: %w", err)
	case !fi.Mode().IsRegular():
		return fmt.Errorf("ops: backup: %s is not a regular file", d.Paths.Secrets)
	}
	data, err := readSmallFile(d.Paths.Secrets, backupMaxSecrets)
	if err != nil {
		return err
	}
	b.add(backupEntry{name: backupSecrets, modTime: fi.ModTime(), size: int64(len(data)), data: data})
	return nil
}

// stageCerts adds the certificate storage: every directory and regular file below certmagic/ except locks/.
// Anything else in there (a symlink, a socket) is not certmagic's and is left out.
func (d *DataDir) stageCerts(ctx context.Context, b *Backup) error {
	root := d.Paths.CertMagic
	if fi, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) || (err == nil && !fi.IsDir()) {
		return nil // off and manual mode keep no certificates here
	} else if err != nil {
		return fmt.Errorf("ops: backup: %w", err)
	}
	var (
		entries []backupEntry
		sums    = map[string]string{}
		total   int64
	)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		// What is gone when the walk gets to it was removed meanwhile: the running server's certmagic cleans up
		// after a renewal.
		if errors.Is(err, fs.ErrNotExist) && path != root {
			return nil
		}
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		name := backupCerts + "/" + filepath.ToSlash(rel)
		fi, err := entry.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			if rel == certLocksDir {
				return fs.SkipDir
			}
			entries = append(entries, backupEntry{name: name, dir: true, modTime: fi.ModTime()})
		case fi.Mode().IsRegular():
			if fi.Size() > backupMaxCertFile || total+fi.Size() > backupMaxCertTotal {
				return fmt.Errorf("the certificate storage %s is larger than a certificate storage should be (%s); "+
					"back up without it: --no-certs", root, path)
			}
			data, err := readSmallFile(path, backupMaxCertFile)
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			total += int64(len(data))
			sum := sha256.Sum256(data)
			sums[name] = hex.EncodeToString(sum[:])
			entries = append(entries, backupEntry{name: name, modTime: fi.ModTime(), size: int64(len(data)), data: data})
		default:
			d.log().Debug("left out of the backup: not a regular file", slog.String("path", path))
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("ops: backup: certificate storage: %w", err)
	}
	// A storage without a single file holds no certificate: the archive then says nothing about certificates, and
	// a restore leaves the ones it finds alone.
	if len(sums) == 0 {
		return nil
	}
	b.Manifest.Contents = append(b.Manifest.Contents, backupCerts+"/")
	for name, sum := range sums {
		b.Manifest.SHA256[name] = sum
	}
	b.entries = append(b.entries, entries...)
	return nil
}

// add appends a top-level file to the archive and to the manifest.
func (b *Backup) add(e backupEntry) {
	if e.path == "" {
		sum := sha256.Sum256(e.data)
		e.sum = hex.EncodeToString(sum[:])
	}
	b.Manifest.SHA256[e.name] = e.sum
	b.Manifest.Contents = append(b.Manifest.Contents, e.name)
	b.entries = append(b.entries, e)
}

// fileEntry is the entry of a file that stays on disk, unchanged, until the archive is written: the snapshot, or a
// raw file of a stopped server. It reads the file once, for its hash.
func fileEntry(ctx context.Context, name, path string) (backupEntry, error) {
	f, err := os.Open(path) //nolint:gosec // G304: a file of the data directory
	if err != nil {
		return backupEntry{}, fmt.Errorf("ops: backup: %w", err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return backupEntry{}, fmt.Errorf("ops: backup: %w", err)
	}
	h := sha256.New()
	n, err := copyContext(ctx, h, f)
	if err != nil {
		return backupEntry{}, fmt.Errorf("ops: backup: reading %s: %w", path, err)
	}
	return backupEntry{name: name, modTime: fi.ModTime(), size: n, path: path, sum: hex.EncodeToString(h.Sum(nil))}, nil
}

// WriteTo writes the archive to w: gzip around tar, manifest.json first, every entry with mode 0600 (directories
// 0700) and no owner, since the files belong to whoever runs the server that restores them. An error leaves w
// with a part of an archive; the caller discards it.
func (b *Backup) WriteTo(ctx context.Context, w io.Writer) error {
	manifest, err := json.MarshalIndent(b.Manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("ops: backup: manifest: %w", err)
	}
	manifest = append(manifest, '\n')

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	created := time.Time(b.Manifest.CreatedAt)
	if err := writeTarFile(ctx, tw, backupManifest, created, int64(len(manifest)), strings.NewReader(string(manifest))); err != nil {
		return err
	}
	for _, e := range b.entries {
		switch {
		case e.dir:
			err = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: e.name + "/", Mode: 0o700, ModTime: e.modTime})
		case e.path != "":
			err = func() error {
				f, err := os.Open(e.path)
				if err != nil {
					return err
				}
				defer func() { _ = f.Close() }()
				return writeTarFile(ctx, tw, e.name, e.modTime, e.size, f)
			}()
		default:
			err = writeTarFile(ctx, tw, e.name, e.modTime, e.size, strings.NewReader(string(e.data)))
		}
		if err != nil {
			return storageError(fmt.Errorf("ops: backup: writing %s: %w", e.name, err))
		}
	}
	if err := tw.Close(); err != nil {
		return storageError(fmt.Errorf("ops: backup: %w", err))
	}
	if err := gz.Close(); err != nil {
		return storageError(fmt.Errorf("ops: backup: %w", err))
	}
	return nil
}

// writeTarFile writes one regular file of size bytes from r.
func writeTarFile(ctx context.Context, tw *tar.Writer, name string, modTime time.Time, size int64, r io.Reader) error {
	hdr := &tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o600, Size: size, ModTime: modTime}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	n, err := copyContext(ctx, tw, r)
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("%s changed while it was backed up (%d bytes, then %d)", name, size, n)
	}
	return nil
}

// WriteBackupFile writes the archive of b to a new file at path (mode 0600, synced with its directory) and removes
// the file again when that fails. It is how a backup lands on the server's own disk: the pre-restore backup, and
// `isshoni admin backup --offline --out PATH`. An existing file is an error (fs.ErrExist): a backup never
// overwrites another.
func (b *Backup) WriteBackupFile(ctx context.Context, path string) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: the operator's or our own path
	if err != nil {
		return storageError(fmt.Errorf("ops: backup: %w", err))
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(path)
		}
	}()
	if err := b.WriteTo(ctx, f); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return storageError(fmt.Errorf("ops: backup: %w", err))
	}
	if err := f.Close(); err != nil {
		return storageError(fmt.Errorf("ops: backup: %w", err))
	}
	return syncDir(filepath.Dir(path))
}

// ReadManifest reads the manifest of the backup archive in r: the first entry of the tar inside the gzip stream.
// It reads no further, so `isshoni admin restore` can show what a file holds before it asks. Its error is a
// *DataError with backup_invalid when r is not a backup archive.
func ReadManifest(r io.Reader) (Manifest, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return Manifest{}, invalidBackup("it is not a gzip file", err)
	}
	defer func() { _ = gz.Close() }()
	return readManifest(tar.NewReader(gz))
}

// readManifest reads and checks the first entry of an archive.
func readManifest(tr *tar.Reader) (Manifest, error) {
	hdr, err := tr.Next()
	if err != nil {
		return Manifest{}, invalidBackup("it is not a tar archive", err)
	}
	if hdr.Typeflag != tar.TypeReg || hdr.Name != backupManifest {
		return Manifest{}, invalidBackup("its first entry is "+quoteName(hdr.Name)+", not "+backupManifest, nil)
	}
	if hdr.Size > restoreMaxManifestBytes {
		return Manifest{}, invalidBackup("its manifest is too large", nil)
	}
	data, err := io.ReadAll(io.LimitReader(tr, restoreMaxManifestBytes+1))
	if err != nil {
		return Manifest{}, invalidBackup("its manifest can't be read", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, invalidBackup("its manifest is not valid JSON", err)
	}
	switch {
	case m.Format < 1:
		return Manifest{}, invalidBackup(fmt.Sprintf("its manifest has the format %d", m.Format), nil)
	case m.Format > BackupFormat:
		return Manifest{}, newerBackup(m.IsshoniVersion)
	}
	return m, nil
}

// newerBackup is the refusal of a backup that a newer isshoni wrote (409 backup_newer, 04 §12.4). appVersion is the
// version that wrote it, as far as it is known.
func newerBackup(appVersion string) *DataError {
	if appVersion == "" || appVersion == "unknown" {
		return &DataError{
			Code:    api.CodeBackupNewer,
			Message: "This backup is from a newer isshoni than this one (" + version.Version() + "); install that version or a newer one first",
		}
	}
	return &DataError{
		Code:    api.CodeBackupNewer,
		Message: "This backup is from isshoni " + appVersion + "; install " + appVersion + " or newer first",
	}
}

// invalidBackup is the refusal of a file that is no backup this build can restore (400 backup_invalid).
func invalidBackup(why string, err error) *DataError {
	return &DataError{
		Code:    api.CodeBackupInvalid,
		Message: "this is not a backup that isshoni can restore: " + why,
		Fix:     "pass a backup archive written by `isshoni admin backup`, or a backups/pre-*.db database file",
		Err:     err,
	}
}

// errNoSpace stands in for the kernel's "no space left on device" where a free-space check sees it coming.
var errNoSpace = errors.New("not enough free disk space")

// noSpace reports whether err says "the disk is full": the kernel's error, or a free-space check of ours.
func noSpace(err error) bool { return errors.Is(err, syscall.ENOSPC) || errors.Is(err, errNoSpace) }

// storageError turns an error that says "the disk is full" into the refusal 507 insufficient_storage; any other
// error is returned as it is.
func storageError(err error) error {
	var de *DataError
	if err == nil || errors.As(err, &de) || !noSpace(err) {
		return err
	}
	return &DataError{
		Code:    api.CodeInsufficientStorage,
		Message: "not enough free disk space in the data directory",
		Fix:     "free some space (old files in the data directory's backups/ are safe to move away), then try again",
		Err:     err,
	}
}

// readSmallFile reads a file of at most limit bytes.
func readSmallFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // G304: a file of the data directory
	if err != nil {
		return nil, fmt.Errorf("ops: backup: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fmt.Errorf("ops: backup: reading %s: %w", path, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("ops: backup: %s is larger than %d bytes", path, limit)
	}
	return data, nil
}

// copyContext is io.Copy that gives up when ctx ends: a backup of a large database can be interrupted.
func copyContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 256<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
		}
		if errors.Is(err, io.EOF) {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

// syncDir makes the directory's entries durable: a new file or a rename survives a crash only then.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil // a directory can't be synced there
	}
	f, err := os.Open(dir) //nolint:gosec // G304: a directory of the data directory, or the operator's
	if err != nil {
		return fmt.Errorf("ops: sync %s: %w", dir, err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Sync(); err != nil && !errors.Is(err, errors.ErrUnsupported) && !errors.Is(err, syscall.EINVAL) {
		return fmt.Errorf("ops: sync %s: %w", dir, err)
	}
	return nil
}

// randomName returns eight hex digits for the name of a temporary file.
func randomName() string {
	var b [4]byte
	_, _ = rand.Read(b[:]) // never fails (crypto/rand)
	return hex.EncodeToString(b[:])
}

// quoteName prints a name from an archive for a message: quoted, and cut when it is absurdly long.
func quoteName(name string) string {
	if len(name) > 80 {
		name = name[:80] + "…"
	}
	return fmt.Sprintf("%q", name)
}
