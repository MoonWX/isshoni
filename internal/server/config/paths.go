package config

import "path/filepath"

// Paths are the files and directories of the data directory (§5.1).
type Paths struct {
	DataDir   string // data_dir itself (0700)
	DB        string // isshoni.db (03; -wal and -shm next to it)
	Secrets   string // secrets.json (0600, §5.2)
	CertMagic string // certmagic/: certificates, ACME account keys, locks
	Backups   string // backups/: pre-migration and pre-restore backups
	Restore   string // restore/: transient staging for restores
	Lock      string // isshoni.lock: the data-directory lock (§5.1)
}

// Paths returns the data-directory layout under data_dir. A relative data_dir (dev only) stays relative, so it
// resolves against the working directory.
func (c *Config) Paths() Paths {
	d := c.DataDir
	return Paths{
		DataDir:   d,
		DB:        filepath.Join(d, "isshoni.db"),
		Secrets:   filepath.Join(d, "secrets.json"),
		CertMagic: filepath.Join(d, "certmagic"),
		Backups:   filepath.Join(d, "backups"),
		Restore:   filepath.Join(d, "restore"),
		Lock:      filepath.Join(d, "isshoni.lock"),
	}
}
