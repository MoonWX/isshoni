package tlsmgr

import (
	"context"
	"crypto/x509"
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/server/config"
)

const testDomain = "watch.example.com"

// manualManager builds a manual-mode manager on files.
func manualManager(t *testing.T, files *certFiles) (*Manager, *logSink) {
	t.Helper()
	return newManager(t, Options{
		Mode:     config.TLSManual,
		Domain:   testDomain,
		CertFile: files.cert,
		KeyFile:  files.key,
		Site:     testSite(config.TLSManual, testDomain),
	})
}

func start(t *testing.T, m *Manager) {
	t.Helper()
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
}

func wantReady(t *testing.T, m *Manager) {
	t.Helper()
	if ok, detail := m.Ready(); !ok || detail != "" {
		t.Fatalf("Ready = %v, %q; want ready", ok, detail)
	}
}

func wantNotReady(t *testing.T, m *Manager, detailPart string) {
	t.Helper()
	ok, detail := m.Ready()
	if ok || !strings.Contains(detail, detailPart) {
		t.Fatalf("Ready = %v, %q; want not ready with %q", ok, detail, detailPart)
	}
	if st := m.Status(); st.Ready {
		t.Errorf("Status().Ready is true while Ready() is false")
	}
}

// poll lets the fake clock pass one poll interval and waits for the watcher to finish what it found.
func poll(t *testing.T) {
	t.Helper()
	time.Sleep(manualPollInterval + time.Second)
	synctest.Wait()
}

// TestManualLoad: the pair is loaded at Start, serves handshakes for h2 and http/1.1, and shows in Status.
func TestManualLoad(t *testing.T) {
	now := time.Now()
	ca := newTestCA(t, now)
	files := newCertFiles(t)
	p := ca.issue(t, now.Add(-time.Hour), now.Add(90*24*time.Hour), testDomain, "203.0.113.7")
	files.writePair(t, p)
	m, sink := manualManager(t, files)

	// Before Start there is no certificate: not ready, and a handshake fails.
	wantNotReady(t, m, "no certificate loaded")
	if _, err := handshake(t, m, ca.pool, testDomain); err == nil {
		t.Error("a handshake succeeded before Start")
	}

	start(t, m)
	wantReady(t, m)
	for _, proto := range []string{"h2", "http/1.1"} {
		state, err := handshake(t, m, ca.pool, testDomain, proto)
		if err != nil {
			t.Fatalf("handshake (%s): %v", proto, err)
		}
		if state.NegotiatedProtocol != proto {
			t.Errorf("negotiated %q, want %q", state.NegotiatedProtocol, proto)
		}
		if state.Version < 0x0303 {
			t.Errorf("TLS version %#x, want 1.2 or newer", state.Version)
		}
		if got := state.PeerCertificates[0].SerialNumber; got.Cmp(p.serial) != 0 {
			t.Errorf("served serial %v, want %v", got, p.serial)
		}
	}
	// A client without SNI (it connects to the IP address) gets the same certificate.
	if got := servedSerial(t, m, ca.pool, ""); got.Cmp(p.serial) != 0 {
		t.Errorf("no SNI: served serial %v, want %v", got, p.serial)
	}
	// Manual mode answers no ACME challenge, so it does not offer the protocol.
	if slices.Contains(m.TLSConfig().NextProtos, "acme-tls/1") {
		t.Errorf("NextProtos = %v: acme-tls/1 is for auto and ip mode only", m.TLSConfig().NextProtos)
	}

	st := m.Status()
	if st.Mode != config.TLSManual || !st.Ready || !slices.Equal(st.Names, []string{testDomain, "203.0.113.7"}) {
		t.Errorf("Status = %+v", st)
	}
	if st.Issuer != "isshoni tests Test Root" || !st.NotAfter.Equal(now.Add(90*24*time.Hour).Truncate(time.Second)) || st.NotBefore.IsZero() {
		t.Errorf("Status issuer %q, validity %v to %v", st.Issuer, st.NotBefore, st.NotAfter)
	}
	if !st.NextRenewal.IsZero() || st.LastError != "" || st.LastErrorCode != "" || !st.LastErrorAt.IsZero() {
		t.Errorf("Status = %+v: manual mode has no renewal, and nothing failed", st)
	}
	if len(sink.records(t, "certificate loaded")) != 1 {
		t.Errorf("want one \"certificate loaded\" line:\n%s", sink.String())
	}
	if n := len(sink.records(t, "expires in")) + len(sink.records(t, "does not cover")); n != 0 {
		t.Errorf("a good certificate got %d warnings:\n%s", n, sink.String())
	}
}

// TestManualReloadOnFileChange: the poll loads a changed pair; a bad new pair is rejected once, and the old one
// keeps serving until the files are whole again.
func TestManualReloadOnFileChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now()
		ca := newTestCA(t, now)
		files := newCertFiles(t)
		valid := func() pair { return ca.issue(t, now.Add(-time.Hour), now.Add(90*24*time.Hour), testDomain) }
		a, b, c := valid(), valid(), valid()
		files.writePair(t, a)
		m, sink := manualManager(t, files)
		start(t, m)
		serial := func() string { return servedSerial(t, m, ca.pool, testDomain).String() }
		if got := serial(); got != a.serial.String() {
			t.Fatalf("serving %s, want the first pair %s", got, a.serial)
		}

		// Nothing changed: the poll leaves the pair alone.
		poll(t)
		if n := len(sink.records(t, "certificate loaded")); n != 1 {
			t.Fatalf("%d loads without a change, want 1", n)
		}

		// A new pair is picked up within one interval.
		files.writePair(t, b)
		if got := serial(); got != a.serial.String() {
			t.Fatalf("serving %s before the poll, want still %s", got, a.serial)
		}
		poll(t)
		if got := serial(); got != b.serial.String() {
			t.Fatalf("serving %s after the poll, want the new pair %s", got, b.serial)
		}

		// A renewal tool caught between its two writes: the new certificate with the old key.
		files.write(t, files.cert, c.certPEM)
		poll(t)
		if got := serial(); got != b.serial.String() {
			t.Fatalf("serving %s, want the old pair %s while the new one is broken", got, b.serial)
		}
		wantReady(t, m) // the old certificate is still good
		st := m.Status()
		if st.LastErrorCode != CodeCertInvalid || !strings.Contains(st.LastError, "not a certificate with its private key") || st.LastErrorAt.IsZero() {
			t.Errorf("Status after a mismatched pair = %+v", st)
		}
		rejected := sink.records(t, "new certificate files rejected")
		if len(rejected) != 1 || rejected[0]["code"] != CodeCertInvalid || rejected[0]["level"] != "WARN" {
			t.Fatalf("want one rejection warning with the code, got %v", rejected)
		}
		// The broken pair is not tried again every minute.
		poll(t)
		poll(t)
		if n := len(sink.records(t, "new certificate files rejected")); n != 1 {
			t.Errorf("%d rejection lines for one broken pair, want 1", n)
		}

		// The key arrives: the pair is whole and loads.
		files.write(t, files.key, c.keyPEM)
		poll(t)
		if got := serial(); got != c.serial.String() {
			t.Fatalf("serving %s, want the completed pair %s", got, c.serial)
		}
		if st := m.Status(); st.LastErrorCode != "" || st.LastError != "" || !st.LastErrorAt.IsZero() {
			t.Errorf("the error stays after a good load: %+v", st)
		}

		// A change of size alone counts too: each file gets its old modification time back.
		var before [2]os.FileInfo
		for i, f := range []string{files.cert, files.key} {
			fi, err := os.Stat(f)
			if err != nil {
				t.Fatal(err)
			}
			before[i] = fi
		}
		padded := append(slices.Clone(a.certPEM), '\n')
		if int64(len(padded)) == before[0].Size() {
			padded = append(padded, '\n')
		}
		files.write(t, files.cert, padded)
		files.write(t, files.key, a.keyPEM)
		for i, f := range []string{files.cert, files.key} {
			if err := os.Chtimes(f, before[i].ModTime(), before[i].ModTime()); err != nil {
				t.Fatal(err)
			}
		}
		poll(t)
		if got := serial(); got != a.serial.String() {
			t.Fatalf("serving %s, want %s after a change of size", got, a.serial)
		}
	})
}

// TestManualReload: Reload (SIGHUP) loads the files at once, and returns the error of a pair it rejects.
func TestManualReload(t *testing.T) {
	now := time.Now()
	ca := newTestCA(t, now)
	files := newCertFiles(t)
	valid := func() pair { return ca.issue(t, now.Add(-time.Hour), now.Add(90*24*time.Hour), testDomain) }
	a, b := valid(), valid()
	files.writePair(t, a)
	m, sink := manualManager(t, files)
	start(t, m)

	files.writePair(t, b)
	if err := m.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := servedSerial(t, m, ca.pool, testDomain); got.Cmp(b.serial) != 0 {
		t.Fatalf("serving %v after Reload, want %v", got, b.serial)
	}

	// A key that belongs to another certificate.
	files.write(t, files.key, a.keyPEM)
	err := m.Reload()
	var lerr *loadError
	if !errors.As(err, &lerr) || lerr.code != CodeCertInvalid {
		t.Fatalf("Reload with a mismatched key = %v, want a %s error", err, CodeCertInvalid)
	}
	if got := servedSerial(t, m, ca.pool, testDomain); got.Cmp(b.serial) != 0 {
		t.Errorf("serving %v after a rejected Reload, want still %v", got, b.serial)
	}
	wantReady(t, m)

	// A file that is gone.
	if err := os.Remove(files.cert); err != nil {
		t.Fatal(err)
	}
	err = m.Reload()
	if !errors.As(err, &lerr) || lerr.code != CodeCertUnreadable || !errors.Is(err, fs.ErrNotExist) ||
		!strings.Contains(err.Error(), "tls.cert_file = "+files.cert+" does not exist") {
		t.Fatalf("Reload without the certificate file = %v", err)
	}
	if st := m.Status(); st.LastErrorCode != CodeCertUnreadable || !st.Ready {
		t.Errorf("Status = %+v: want the read error, and still ready on the old pair", st)
	}

	// Not PEM at all.
	files.write(t, files.cert, []byte("not a certificate\n"))
	if err := m.Reload(); !errors.As(err, &lerr) || lerr.code != CodeCertInvalid {
		t.Fatalf("Reload with garbage = %v, want a %s error", err, CodeCertInvalid)
	}

	files.writePair(t, a)
	if err := m.Reload(); err != nil {
		t.Fatalf("Reload with a good pair again: %v", err)
	}
	if got := servedSerial(t, m, ca.pool, testDomain); got.Cmp(a.serial) != 0 {
		t.Errorf("serving %v, want %v", got, a.serial)
	}
	if n := len(sink.records(t, "new certificate files rejected")); n != 3 {
		t.Errorf("%d rejection lines, want 3:\n%s", n, sink.String())
	}
}

// TestManualKeyMismatchAtStart: with a pair that does not match there is no certificate. The server starts, is not
// ready, and becomes ready when the files are fixed.
func TestManualKeyMismatchAtStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now()
		ca := newTestCA(t, now)
		files := newCertFiles(t)
		a := ca.issue(t, now.Add(-time.Hour), now.Add(90*24*time.Hour), testDomain)
		other := ca.issue(t, now.Add(-time.Hour), now.Add(90*24*time.Hour), testDomain)
		files.write(t, files.cert, a.certPEM)
		files.write(t, files.key, other.keyPEM)
		m, sink := manualManager(t, files)

		start(t, m) // not an error of Start
		wantNotReady(t, m, "no certificate loaded from tls.cert_file ("+CodeCertInvalid+")")
		if _, err := handshake(t, m, ca.pool, testDomain); err == nil {
			t.Error("a handshake succeeded without a certificate")
		}
		st := m.Status()
		if st.LastErrorCode != CodeCertInvalid || len(st.Names) != 0 || st.Names == nil || !st.NotAfter.IsZero() {
			t.Errorf("Status = %+v", st)
		}
		if recs := sink.records(t, "no certificate loaded"); len(recs) != 1 || recs[0]["level"] != "ERROR" || recs[0]["component"] != "tls" {
			t.Errorf("want one error line with component=tls, got %v", recs)
		}

		files.write(t, files.key, a.keyPEM)
		poll(t)
		wantReady(t, m)
		if got := servedSerial(t, m, ca.pool, testDomain); got.Cmp(a.serial) != 0 {
			t.Errorf("serving %v, want %v", got, a.serial)
		}
	})
}

// TestManualExpiry: the 14-day warning, a certificate that runs out while it serves, an expired one at start, and
// an expired replacement of a good one.
func TestManualExpiry(t *testing.T) {
	t.Run("warning within 14 days", func(t *testing.T) {
		now := time.Now()
		ca := newTestCA(t, now)
		files := newCertFiles(t)
		files.writePair(t, ca.issue(t, now.Add(-80*24*time.Hour), now.Add(10*24*time.Hour+time.Hour), testDomain))
		m, sink := manualManager(t, files)
		start(t, m)
		wantReady(t, m)
		recs := sink.records(t, "the certificate expires in 10 days")
		if len(recs) != 1 || recs[0]["level"] != "WARN" {
			t.Errorf("want the expiry warning, got:\n%s", sink.String())
		}
	})

	t.Run("runs out while serving", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			now := time.Now()
			ca := newTestCA(t, now)
			files := newCertFiles(t)
			files.writePair(t, ca.issue(t, now.Add(-time.Hour), now.Add(time.Hour), testDomain))
			m, _ := manualManager(t, files)
			start(t, m)
			wantReady(t, m)

			time.Sleep(2 * time.Hour)
			wantNotReady(t, m, "the certificate expired on 2000-01-01")
			// It still serves, so that clients see why.
			var invalid x509.CertificateInvalidError
			if _, err := handshake(t, m, ca.pool, testDomain); !errors.As(err, &invalid) || invalid.Reason != x509.Expired {
				t.Errorf("handshake error = %v, want an expired certificate", err)
			}
		})
	})

	t.Run("expired at start", func(t *testing.T) {
		now := time.Now()
		ca := newTestCA(t, now)
		files := newCertFiles(t)
		old := ca.issue(t, now.Add(-90*24*time.Hour), now.Add(-24*time.Hour), testDomain)
		files.writePair(t, old)
		m, sink := manualManager(t, files)
		start(t, m)

		wantNotReady(t, m, "the certificate expired on "+now.Add(-24*time.Hour).UTC().Format(time.DateOnly))
		st := m.Status()
		if st.LastErrorCode != CodeCertExpired || !strings.Contains(st.LastError, "expired on") || st.NotAfter.IsZero() {
			t.Errorf("Status = %+v", st)
		}
		if len(sink.records(t, "it is not valid now")) != 1 {
			t.Errorf("want the error line:\n%s", sink.String())
		}
		// A good pair replaces it.
		fresh := ca.issue(t, now.Add(-time.Hour), now.Add(90*24*time.Hour), testDomain)
		files.writePair(t, fresh)
		if err := m.Reload(); err != nil {
			t.Fatal(err)
		}
		wantReady(t, m)
		if st := m.Status(); st.LastErrorCode != "" {
			t.Errorf("the error stays: %+v", st)
		}

		// An expired pair does not replace a good one.
		files.writePair(t, old)
		var lerr *loadError
		if err := m.Reload(); !errors.As(err, &lerr) || lerr.code != CodeCertExpired {
			t.Fatalf("Reload with an expired pair = %v", err)
		}
		wantReady(t, m)
		if got := servedSerial(t, m, ca.pool, testDomain); got.Cmp(fresh.serial) != 0 {
			t.Errorf("serving %v, want the good pair %v", got, fresh.serial)
		}
	})

	t.Run("not valid yet", func(t *testing.T) {
		now := time.Now()
		ca := newTestCA(t, now)
		files := newCertFiles(t)
		files.writePair(t, ca.issue(t, now.Add(48*time.Hour), now.Add(90*24*time.Hour), testDomain))
		m, _ := manualManager(t, files)
		start(t, m)
		wantNotReady(t, m, "the certificate is not valid before")
		if st := m.Status(); st.LastErrorCode != CodeCertExpired || !strings.Contains(st.LastError, "Check the server's clock") {
			t.Errorf("Status = %+v", st)
		}
	})
}

// TestManualNameMismatchWarns: a certificate for another name loads (the operator may know better), with a warning.
func TestManualNameMismatchWarns(t *testing.T) {
	now := time.Now()
	ca := newTestCA(t, now)
	files := newCertFiles(t)
	files.writePair(t, ca.issue(t, now.Add(-time.Hour), now.Add(90*24*time.Hour), "other.example.com"))
	m, sink := manualManager(t, files)
	start(t, m)
	wantReady(t, m)
	recs := sink.records(t, "does not cover the server's name")
	if len(recs) != 1 || recs[0]["level"] != "WARN" || recs[0]["name"] != testDomain {
		t.Errorf("want the name warning, got:\n%s", sink.String())
	}
	if st := m.Status(); !slices.Equal(st.Names, []string{"other.example.com"}) {
		t.Errorf("Status.Names = %v", st.Names)
	}
}

// TestManualUnreadableFile: a file the service user may not read is an error with the fix (04 §8.4).
func TestManualUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every file")
	}
	now := time.Now()
	ca := newTestCA(t, now)
	files := newCertFiles(t)
	files.writePair(t, ca.issue(t, now.Add(-time.Hour), now.Add(90*24*time.Hour), testDomain))
	if err := os.Chmod(files.key, 0); err != nil {
		t.Fatal(err)
	}
	m, _ := manualManager(t, files)
	start(t, m)
	wantNotReady(t, m, CodeCertUnreadable)
	st := m.Status()
	if st.LastErrorCode != CodeCertUnreadable || !strings.Contains(st.LastError, "isshoni may not read tls.key_file = "+files.key) ||
		!strings.Contains(st.LastError, "chmod 640") {
		t.Errorf("Status = %+v", st)
	}
}

// TestManualShutdownStopsTheWatcher: after Shutdown nothing is loaded any more, and the loaded pair keeps serving.
func TestManualShutdownStopsTheWatcher(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now()
		ca := newTestCA(t, now)
		files := newCertFiles(t)
		a := ca.issue(t, now.Add(-time.Hour), now.Add(90*24*time.Hour), testDomain)
		files.writePair(t, a)
		m, _ := manualManager(t, files)
		start(t, m)
		if err := m.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		files.writePair(t, ca.issue(t, now.Add(-time.Hour), now.Add(90*24*time.Hour), testDomain))
		poll(t)
		if got := servedSerial(t, m, ca.pool, testDomain); got.Cmp(a.serial) != 0 {
			t.Errorf("serving %v after Shutdown, want still %v", got, a.serial)
		}
		if err := m.Start(context.Background()); !errors.Is(err, errStopped) {
			t.Errorf("Start after Shutdown = %v, want errStopped", err)
		}
		if err := m.Shutdown(context.Background()); err != nil {
			t.Errorf("second Shutdown: %v", err)
		}
	})
}
