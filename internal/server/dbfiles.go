package server

import (
	"context"
	"errors"
	"fmt"
	"syscall"

	"github.com/MoonWX/isshoni/internal/server/ops/doctor"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// DBFiles returns 03's file-level store functions in the form that ops and ops/doctor take them (04 §6.6): neither
// may import store (04 §2), so the wiring passes them as function values, and cmd/isshoni does the same for the
// offline commands (offline doctor, `admin backup --offline`, `admin restore --offline`). None of the three
// migrates a database or writes to one, so they are safe on the files of a stopped server, and on a file that a
// restore is still deciding about.
//
// backupDir is the data directory's backups/ (config.Paths.Backups): Inspect only reads it, to name the
// pre-migration backup in the message about a database with a newer schema.
//
// Backup's error for a full disk wraps syscall.ENOSPC (diskFull), which is how ops tells it from a database that
// can't be read.
func DBFiles(backupDir string) doctor.DBFiles {
	return doctor.DBFiles{
		LatestSchemaVersion: store.LatestSchemaVersion,
		Inspect: func(ctx context.Context, dbPath string) (doctor.DBInfo, error) {
			info, err := store.InspectFile(ctx, dbPath, backupDir)
			if err != nil {
				return doctor.DBInfo{}, err
			}
			out := doctor.DBInfo{
				SchemaVersion:  info.SchemaVersion,
				LastAppVersion: info.LastAppVersion,
				HistoryOK:      info.HistoryOK,
				Integrity:      info.Integrity,
			}
			if info.TooNew != nil {
				out.TooNewBackup, out.TooNewMessage = info.TooNew.Backup, info.TooNew.Error()
			}
			return out, nil
		},
		Backup: func(ctx context.Context, src, dst string) error {
			return diskFull(store.BackupFile(ctx, src, dst))
		},
	}
}

// sqliteFull is SQLite's result code SQLITE_FULL, "database or disk is full". An extended result code carries the
// primary one in its low byte.
const sqliteFull = 13

// diskFull returns err, from a snapshot of the database (VACUUM INTO), so that a full disk reads as one: when SQLite
// ran out of disk, the result also wraps syscall.ENOSPC. The driver's error has the result code and does not unwrap
// to the kernel's error, and ops, which answers a full disk with 507 insufficient_storage (04 §12.2), may not know
// SQLite (04 §2). Any other error is returned as it is.
func diskFull(err error) error {
	var coded interface{ Code() int }
	if err == nil || errors.Is(err, syscall.ENOSPC) || !errors.As(err, &coded) || coded.Code()&0xff != sqliteFull {
		return err
	}
	return fmt.Errorf("%w (%w)", err, syscall.ENOSPC)
}
