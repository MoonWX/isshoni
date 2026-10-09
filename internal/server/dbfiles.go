package server

import (
	"context"

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
		Backup: store.BackupFile,
	}
}
