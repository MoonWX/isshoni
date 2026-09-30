package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

var (
	// ErrNotFound means the row does not exist (or, for methods documented so, is not in the required state).
	ErrNotFound = errors.New("store: not found")
	// ErrConflict is wrapped by *ConflictError: a UNIQUE constraint rejected the write.
	ErrConflict = errors.New("store: unique constraint")
	// ErrInviteUnusable is returned by UseInvite when the invite is expired, used up or revoked.
	ErrInviteUnusable = errors.New("store: invite expired, used up or revoked")
	// ErrDefaultRoom is returned by DeleteRoom for the default room.
	ErrDefaultRoom = errors.New("store: the default room cannot be deleted")
	// ErrNeedsOperator is wrapped by every Open error that a restart can't fix (03 §4.3): a schema newer than the
	// binary (*SchemaTooNewError), a schema history mismatch, a failed migration step or foreign key check, a
	// corrupt file or one that is not a database, WAL mode unavailable, and too little free space for the
	// pre-migration backup. The server exits 78 exactly when errors.Is(err, ErrNeedsOperator).
	ErrNeedsOperator = errors.New("store: needs operator action")
)

// errNotImplemented marks the parts of the public API that later slices fill in. Callers see a clear error instead
// of a silent no-op.
var errNotImplemented = errors.New("not implemented yet")

// errClosed is returned by every DB method called after Close.
var errClosed = errors.New("store: database is closed")

// errReadOnlyTx is returned by a write method called inside Read.
var errReadOnlyTx = errors.New("store: write inside a read transaction")

func notImplemented(method string) error {
	return fmt.Errorf("store: %s: %w", method, errNotImplemented)
}

// ConflictError reports the UNIQUE column that rejected a write.
type ConflictError struct {
	Column string // "username_key" | "name_key" | "endpoint"
}

func (e *ConflictError) Error() string { return "store: " + e.Column + " is already taken" }

// Unwrap returns ErrConflict.
func (e *ConflictError) Unwrap() error { return ErrConflict }

// SchemaTooNewError is returned by Open (and reported by InspectFile) when the file's schema is newer than this
// binary. It wraps ErrNeedsOperator. Error() is the one actionable message for the operator (03 §4.5); the caller
// (04) appends the restore command for its environment.
type SchemaTooNewError struct {
	DBVersion      int    // schema version found in the file
	BinaryVersion  int    // highest version this binary knows
	LastAppVersion string // meta.last_app_version, e.g. "0.6.1"
	Backup         string // newest backups/pre-<BinaryVersion>-*.db, or "" if there is none

	backupDir string // where Backup was looked for, for the message when there is none
}

func (e *SchemaTooNewError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "database schema %d is newer than this isshoni build supports (%d); ", e.DBVersion, e.BinaryVersion)
	if e.LastAppVersion == "" || e.LastAppVersion == unknownAppVersion {
		b.WriteString("it was last used by a newer isshoni. Install a newer isshoni")
	} else {
		fmt.Fprintf(&b, "it was last used by isshoni %s. Install isshoni %s or newer", e.LastAppVersion, e.LastAppVersion)
	}
	switch {
	case e.Backup != "":
		fmt.Fprintf(&b, ", or restore the database from before the upgrade: %s (changes made after that backup are lost)",
			e.Backup)
	case e.backupDir != "":
		fmt.Fprintf(&b, "; no backup for schema %d was found in %s", e.BinaryVersion, e.backupDir)
	default:
		fmt.Fprintf(&b, "; no backup for schema %d was found", e.BinaryVersion)
	}
	return b.String()
}

// Unwrap returns ErrNeedsOperator.
func (e *SchemaTooNewError) Unwrap() error { return ErrNeedsOperator }

// needsOperator wraps err with msg and ErrNeedsOperator, keeping the message first:
// "<msg>: <err>: store: needs operator action".
func needsOperator(msg string, err error) error {
	if err == nil {
		return fmt.Errorf("%s: %w", msg, ErrNeedsOperator)
	}
	return fmt.Errorf("%s: %w: %w", msg, err, ErrNeedsOperator)
}

// sqliteCode returns the primary SQLite result code of err, or 0 if err does not come from SQLite.
func sqliteCode(err error) int {
	var se *sqlite.Error
	if errors.As(err, &se) {
		return se.Code() & 0xff
	}
	return 0
}

// isCorrupt reports whether err is SQLITE_CORRUPT or SQLITE_NOTADB.
func isCorrupt(err error) bool {
	c := sqliteCode(err)
	return c == sqlite3.SQLITE_CORRUPT || c == sqlite3.SQLITE_NOTADB
}

// openErr turns an error from opening or migrating the database into Open's error: context errors stay plain (a
// cancelled start is not an operator problem), a corrupt file or one that is not a database wraps ErrNeedsOperator,
// and everything else is wrapped with msg.
func openErr(ctx context.Context, path, msg string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		if !errors.Is(err, ctxErr) {
			err = fmt.Errorf("%w (%w)", ctxErr, err)
		}
		return fmt.Errorf("%s: %w", msg, err)
	}
	if isCorrupt(err) {
		return needsOperator(fmt.Sprintf("store: %s is corrupt or not an SQLite database (%s)", path, msg), err)
	}
	return fmt.Errorf("%s: %w", msg, err)
}
