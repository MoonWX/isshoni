package tlsmgr

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/MoonWX/isshoni/internal/logx"
)

// Manual mode (04 §8.4): the operator's certificate chain and key, loaded without certmagic (so nothing asks an
// OCSP responder either, 04 §16), kept in an atomic pointer and loaded again when a file changes or Reload asks.

const (
	// manualPollInterval is how often the two files are checked for a new modification time or size. Polling
	// needs no file-watching dependency, and a renewed certificate is in no hurry.
	manualPollInterval = 60 * time.Second
	// expiryWarning is how long before its end a certificate is worth a warning.
	expiryWarning = 14 * 24 * time.Hour
)

// loadedCert is the pair that serves.
type loadedCert struct {
	cert  tls.Certificate
	leaf  *x509.Certificate
	names []string // the leaf's DNS names, then its IP addresses
}

// fileStamp is what the poll compares: a file's modification time and size, or the zero value when it can't be
// looked at.
type fileStamp struct {
	modNano int64
	size    int64
}

// loadError is why a pair was not loaded, or why the loaded one does not make the server ready.
type loadError struct {
	hint
	cause error // may be nil
}

func (e *loadError) Error() string { return "tlsmgr: " + e.fix }
func (e *loadError) Unwrap() error { return e.cause }

// attrs are the log attributes of the error: its code, its fix and, when there is one, its cause.
func (e *loadError) attrs() []any {
	out := []any{slog.String("code", e.code), slog.String("fix", e.fix)}
	if e.cause != nil {
		out = append(out, logx.Err(e.cause))
	}
	return out
}

// loadManual reads tls.cert_file and tls.key_file and makes the pair the one that serves. Its checks (04 §8.4):
//   - both files can be read, and they hold a certificate with its private key: otherwise the pair is rejected;
//   - the certificate is inside its validity: otherwise it is rejected while the old one is still valid, and loaded
//     when there is nothing better (clients then get an "expired" error, which says more than a failed handshake);
//     the server is not ready either way;
//   - it covers the site's host name, and has more than 14 days left: otherwise it is loaded with a warning.
//
// A rejected pair leaves the old one serving. The error, also for a pair that was loaded while outside its
// validity, is a *loadError; it is logged here and kept for Status.
func (m *Manager) loadManual() error {
	m.loadMu.Lock()
	defer m.loadMu.Unlock()
	// The stamps are taken before reading: a file replaced during the read differs from them at the next poll.
	m.seen = m.stamps()
	old := m.manual.Load()
	now := m.now()

	next, lerr := m.readPair()
	if lerr == nil {
		lerr = validity(next.leaf, now)
		if lerr != nil && old != nil && validity(old.leaf, now) == nil {
			next = nil // the old certificate is the better one
		}
	}
	if next == nil {
		m.setError(lerr.hint)
		if old != nil {
			m.log.Warn("new certificate files rejected; the certificate loaded before keeps serving", lerr.attrs()...)
		} else {
			m.log.Error("no certificate loaded; the server is not ready until the files are fixed", lerr.attrs()...)
		}
		return lerr
	}

	m.manual.Store(next)
	attrs := []any{slog.Any("names", next.names), slog.Time("not_after", next.leaf.NotAfter)}
	if lerr != nil {
		m.setError(lerr.hint)
		m.log.Error("certificate loaded, but it is not valid now; the server is not ready", append(attrs, lerr.attrs()...)...)
		return lerr
	}
	m.clearError()
	m.log.Info("certificate loaded", attrs...)
	if host := m.opts.Site.Hostname; host != "" && next.leaf.VerifyHostname(host) != nil {
		m.log.Warn("the certificate does not cover the server's name; browsers will show a warning",
			slog.String("name", host), slog.Any("names", next.names),
			slog.String("fix", "use a certificate for "+host+", or set domain to a name the certificate covers"))
	}
	if left := next.leaf.NotAfter.Sub(now); left < expiryWarning {
		m.log.Warn(fmt.Sprintf("the certificate expires in %d days", int(left.Hours()/24)),
			slog.Time("not_after", next.leaf.NotAfter),
			slog.String("fix", "replace tls.cert_file and tls.key_file; isshoni loads new files within a minute"))
	}
	return nil
}

// readPair reads and parses the two files.
func (m *Manager) readPair() (*loadedCert, *loadError) {
	certPEM, lerr := readCertFile("tls.cert_file", m.opts.CertFile)
	if lerr != nil {
		return nil, lerr
	}
	keyPEM, lerr := readCertFile("tls.key_file", m.opts.KeyFile)
	if lerr != nil {
		return nil, lerr
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, &loadError{hint{CodeCertInvalid, "tls.cert_file and tls.key_file are not a certificate with its private key (" +
			strings.TrimPrefix(err.Error(), "tls: ") + "). Put the full chain in tls.cert_file and the matching key in tls.key_file, both as PEM."}, err}
	}
	leaf := pair.Leaf
	if leaf == nil {
		if leaf, err = x509.ParseCertificate(pair.Certificate[0]); err != nil {
			return nil, &loadError{hint{CodeCertInvalid, "tls.cert_file does not start with a certificate that can be parsed."}, err}
		}
	}
	names := append([]string{}, leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		names = append(names, ip.String())
	}
	return &loadedCert{cert: pair, leaf: leaf, names: names}, nil
}

// readCertFile reads one of the two files. Its errors carry the fix (04 §8.4: "files readable by the service
// user").
func readCertFile(key, path string) ([]byte, *loadError) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the operator's own path, from tls.cert_file or tls.key_file
	switch {
	case err == nil:
		return b, nil
	case errors.Is(err, fs.ErrNotExist):
		return nil, &loadError{hint{CodeCertUnreadable, key + " = " + path + " does not exist. Check the path."}, err}
	case errors.Is(err, fs.ErrPermission):
		return nil, &loadError{hint{CodeCertUnreadable, "isshoni may not read " + key + " = " + path +
			". Let the user that isshoni runs as read it, for example: sudo chgrp isshoni " + path + " && sudo chmod 640 " + path}, err}
	default:
		return nil, &loadError{hint{CodeCertUnreadable, "isshoni can't read " + key + " = " + path + "."}, err}
	}
}

// validity reports a certificate that the clock is outside of.
func validity(leaf *x509.Certificate, now time.Time) *loadError {
	switch {
	case now.After(leaf.NotAfter):
		return &loadError{hint{CodeCertExpired, "The certificate in tls.cert_file expired on " +
			leaf.NotAfter.UTC().Format(time.DateOnly) + ". Replace it and its key; isshoni loads new files within a minute."}, nil}
	case now.Before(leaf.NotBefore):
		return &loadError{hint{CodeCertExpired, "The certificate in tls.cert_file is not valid before " +
			leaf.NotBefore.UTC().Format(time.DateOnly) + ". Check the server's clock, or use a certificate that is valid now."}, nil}
	}
	return nil
}

// stamps looks at the two files.
func (m *Manager) stamps() [2]fileStamp {
	var out [2]fileStamp
	for i, path := range []string{m.opts.CertFile, m.opts.KeyFile} {
		if fi, err := os.Stat(path); err == nil {
			out[i] = fileStamp{modNano: fi.ModTime().UnixNano(), size: fi.Size()}
		}
	}
	return out
}

// changed reports whether a file differs from what the last load found. A load that failed is not repeated until
// the files change again, so a broken pair is logged once, not every minute.
func (m *Manager) changed() bool {
	m.loadMu.Lock()
	defer m.loadMu.Unlock()
	return m.stamps() != m.seen
}

// watchManual loads the pair again whenever a file's modification time or size has changed, until ctx ends. A tool
// that replaces the two files one after the other may be caught in between: that load is rejected (the certificate
// and the key don't match), the old pair keeps serving, and the next poll finds the second file changed.
func (m *Manager) watchManual(ctx context.Context) {
	t := time.NewTicker(m.pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if m.changed() {
				_ = m.loadManual() // logged and kept in Status
			}
		}
	}
}
