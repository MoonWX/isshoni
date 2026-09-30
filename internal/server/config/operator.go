package config

import (
	"errors"
	"strings"
)

// ErrNeedsOperator is wrapped by every error of the data-directory checks and of OpenSecrets that a restart can't
// fix (§5.1, §5.2, §6.3): a container without a data volume, a data directory (or lock file) the process can't
// write, an admin socket directory that can't be created, a corrupt or wrongly owned secrets.json. serve exits 78
// exactly when errors.Is(err, ErrNeedsOperator), like store.ErrNeedsOperator, so systemd's
// RestartPreventExitStatus=78 ends the restart loop. Such an error is always an *OperatorError.
var ErrNeedsOperator = errors.New("config: needs operator action")

// Reason is the code of a refusal to start (§6.3). It is used in logs and by doctor; it never changes once released.
type Reason string

// The reasons of this package. 03's store adds schema_newer, migration_failed and db_corrupt (§6.3).
const (
	// ReasonDataNotMounted: in a container, data_dir is not a mounted volume, so the data would be lost with the
	// container (§5.1).
	ReasonDataNotMounted Reason = "data_not_mounted"
	// ReasonDataDirNotWritable: data_dir can't be created, is not a directory, or the process can't write to it or
	// open its lock file (§5.1).
	ReasonDataDirNotWritable Reason = "data_dir_not_writable"
	// ReasonAdminSocketDir: the directory of listen.admin_socket can't be created (§5.1), for example /run/isshoni in
	// a container with a read-only root filesystem and no tmpfs there.
	ReasonAdminSocketDir Reason = "admin_socket_dir"
	// ReasonSecretsCorrupt: secrets.json can't be parsed or holds an invalid key. It is never regenerated, because a
	// new session key would log everyone out (§5.2).
	ReasonSecretsCorrupt Reason = "secrets_corrupt"
	// ReasonSecretsOwner: secrets.json is owned by another uid than the process's (§5.2).
	ReasonSecretsOwner Reason = "secrets_owner"
)

// OperatorError is a data-directory or secrets.json problem that a restart can't fix. It wraps ErrNeedsOperator and,
// when there is one, the underlying error. Error prints the message and the fix on two lines, like a config
// Problem (§4.5), but leaves the fix as it is (not wrapped, no period added), so a command in it can be copied:
//
//	/var/lib/isshoni/secrets.json is owned by uid 0, but isshoni runs as uid 998.
//	  fix: run: sudo chown isshoni:isshoni /var/lib/isshoni/secrets.json
type OperatorError struct {
	Reason  Reason
	Path    string // the file or directory concerned
	Message string // what is wrong, starting with the path
	Fix     string // what the operator runs or changes
	Err     error  // the underlying error, or nil
}

func (e *OperatorError) Error() string {
	var b strings.Builder
	b.WriteString(sentence(e.Message))
	if e.Fix != "" {
		b.WriteString("\n  fix: " + e.Fix)
	}
	return b.String()
}

// Unwrap returns ErrNeedsOperator and the underlying error.
func (e *OperatorError) Unwrap() []error {
	if e.Err == nil {
		return []error{ErrNeedsOperator}
	}
	return []error{ErrNeedsOperator, e.Err}
}

// ReasonOf returns the Reason of the *OperatorError in err's chain, or "" when there is none.
func ReasonOf(err error) Reason {
	var oe *OperatorError
	if errors.As(err, &oe) {
		return oe.Reason
	}
	return ""
}

// sentence ends s with a period unless it already ends a sentence.
func sentence(s string) string {
	if s == "" || strings.HasSuffix(s, ".") || strings.HasSuffix(s, "?") || strings.HasSuffix(s, ")") {
		return s
	}
	return s + "."
}
