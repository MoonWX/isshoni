//go:build windows

package credstore

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// These tests write real Credential Manager entries, always under a random "isshoni-test:<random>:" prefix that
// they delete afterwards, never under the app's "isshoni:v1:" (02 §16.1). The windows-go CI job runs them.

const unavailableSkip = "Credential Manager is unavailable: this process has no user profile loaded " +
	"(ERROR_NO_SUCH_LOGON_SESSION); the manual check M-DA-1 covers the store"

// openTestStore opens a store under a fresh random prefix and deletes everything under it when the test ends. It
// skips the test when Credential Manager is unavailable.
func openTestStore(t *testing.T, log *slog.Logger) *winStore {
	t.Helper()
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	if log == nil {
		log = discardLogger()
	}
	st, err := Open(Options{Prefix: "isshoni-test:" + hex.EncodeToString(random) + ":", Logger: log})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s := st.(*winStore)
	if _, err := s.List(); errors.Is(err, ErrUnavailable) {
		t.Skip(unavailableSkip)
	} else if err != nil {
		t.Fatalf("List: %v", err)
	}
	t.Cleanup(func() {
		origins, err := s.List()
		if err != nil {
			t.Errorf("cleanup: List: %v", err)
			return
		}
		for _, o := range origins {
			if err := s.Delete(o); err != nil {
				t.Errorf("cleanup: Delete: %v", err)
			}
		}
	})
	return s
}

func TestWinStore(t *testing.T) {
	testStoreContract(t, func(t *testing.T) Store { return openTestStore(t, nil) })
}

func TestWinStoreCredentialFields(t *testing.T) {
	s := openTestStore(t, nil)
	mustPut(t, s, originA, sample("isr_test-fields"))
	raw, err := s.read(s.prefix + originA)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if raw.Type != credTypeGeneric {
		t.Errorf("Type = %d, want CRED_TYPE_GENERIC", raw.Type)
	}
	if raw.Persist != credPersistLocalMachine {
		t.Errorf("Persist = %d, want CRED_PERSIST_LOCAL_MACHINE", raw.Persist)
	}
	if raw.TargetName != s.prefix+originA {
		t.Errorf("TargetName = %q, want %q", raw.TargetName, s.prefix+originA)
	}
	if raw.Comment != "isshoni desktop app sign-in" {
		t.Errorf("Comment = %q", raw.Comment)
	}
	if raw.UserName != "Alex" {
		t.Errorf("UserName = %q, want Alex", raw.UserName)
	}
	const want = `{"v":1,"refreshToken":"isr_test-fields","deviceId":"m4q8x2c6v0bn","savedAt":"2026-11-02T19:05:01.004Z"}`
	if string(raw.Blob) != want {
		t.Errorf("blob:\n got %s\nwant %s", raw.Blob, want)
	}

	// Without a user name, the credential has none.
	noUser := sample("isr_test-no-user")
	noUser.UserName = ""
	mustPut(t, s, originB, noUser)
	assertCredential(t, mustGet(t, s, originB), noUser)
}

func TestWinStoreUnreadableBlob(t *testing.T) {
	const token = "isr_test-unreadable-blob" //nolint:gosec // G101: made up, what the log must not repeat
	blobs := map[string]string{
		"not JSON":  `{"v":1,"refreshToken":"` + token,
		"another v": `{"v":2,"refreshToken":"` + token + `","deviceId":"d","savedAt":"2026-11-02T19:05:01.004Z"}`,
	}
	for name, b := range blobs {
		t.Run(name, func(t *testing.T) {
			log, buf := newLogBuffer()
			s := openTestStore(t, log)
			if err := s.write(s.prefix+originA, "Alex", []byte(b)); err != nil {
				t.Fatalf("write: %v", err)
			}
			if _, err := s.Get(originA); !errors.Is(err, ErrNotFound) {
				t.Errorf("Get: err = %v, want ErrNotFound", err)
			}
			recs := buf.records(t, "WARN", "can't be read")
			if len(recs) != 1 || recs[0]["origin"] != originA {
				t.Errorf("want one WARN naming the origin: %s", buf.String())
			}
			if strings.Contains(buf.String(), token) {
				t.Errorf("the log carries the token: %s", buf.String())
			}
			assertList(t, s, originA) // still listed, so ForgetAll deletes it
			if err := s.Delete(originA); err != nil {
				t.Fatal(err)
			}
			assertList(t, s)
		})
	}
}

func TestWinStoreBlobSizeCheckedBeforeTheCall(t *testing.T) {
	s := openTestStore(t, nil)
	big := []byte(strings.Repeat("a", maxBlobBytes+1))
	if err := s.write(s.prefix+originA, "", big); !errors.Is(err, errBlobTooLarge) {
		t.Fatalf("write of %d bytes: err = %v, want errBlobTooLarge", len(big), err)
	}
	if _, err := s.read(s.prefix + originA); !errors.Is(err, ErrNotFound) {
		t.Errorf("read after a refused write: err = %v, want ErrNotFound", err)
	}
}

func TestWinStorePrefixesAreSeparate(t *testing.T) {
	a, b := openTestStore(t, nil), openTestStore(t, nil)
	mustPut(t, a, originA, sample("isr_test-a"))
	mustPut(t, b, originB, sample("isr_test-b"))
	assertList(t, a, originA)
	assertList(t, b, originB)
	if _, err := b.Get(originA); !errors.Is(err, ErrNotFound) {
		t.Errorf("a store sees another prefix's credential: err = %v", err)
	}
}

func TestWinError(t *testing.T) {
	err := winError("CredReadW", windows.ERROR_NO_SUCH_LOGON_SESSION)
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("ERROR_NO_SUCH_LOGON_SESSION → %v, want ErrUnavailable", err)
	}
	if !strings.Contains(err.Error(), "CredReadW: 0x00000520") {
		t.Errorf("message %q lacks the call and the number", err)
	}

	err = winError("CredWriteW", windows.ERROR_ACCESS_DENIED)
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, ErrUnavailable) {
		t.Errorf("ERROR_ACCESS_DENIED → %v, want it wrapped as it is", err)
	}
	if !strings.HasPrefix(err.Error(), "credstore: CredWriteW: 0x00000005: ") {
		t.Errorf("message = %q, want it to start with the call and the number", err)
	}
}

func TestCredentialWLayout(t *testing.T) {
	// CREDENTIALW as the C compiler lays it out (wincred.h).
	want := map[uintptr][2]uintptr{ // pointer size → {UserName offset, size}
		8: {72, 80},
		4: {48, 52},
	}[unsafe.Sizeof(uintptr(0))]
	var c credentialW
	if got := [2]uintptr{unsafe.Offsetof(c.UserName), unsafe.Sizeof(c)}; got != want {
		t.Errorf("CREDENTIALW: UserName offset and size = %v, want %v", got, want)
	}
	if off := unsafe.Offsetof(c.CredentialBlob); unsafe.Sizeof(uintptr(0)) == 8 && off != 40 {
		t.Errorf("CredentialBlob offset = %d, want 40", off)
	}
}
