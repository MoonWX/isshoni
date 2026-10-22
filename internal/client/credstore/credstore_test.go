package credstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Test values. The tokens are made up; nothing here is a real credential.
const (
	originA = "https://watch.example.com"
	originB = "https://alt.example.org:8443"
	originC = "http://127.0.0.1:8080"
)

func sample(token string) Credential {
	return Credential{
		RefreshToken: token,
		DeviceID:     "m4q8x2c6v0bn",
		UserName:     "Alex",
		SavedAt:      time.Date(2026, 11, 2, 19, 5, 1, 4_000_000, time.UTC),
	}
}

// assertCredential fails unless got is want as a Store gives it back (SavedAt in UTC to the millisecond).
func assertCredential(t *testing.T, got, want Credential) {
	t.Helper()
	want = normalize(want)
	if got.RefreshToken != want.RefreshToken {
		t.Errorf("RefreshToken differs: got %d bytes, want %d bytes", len(got.RefreshToken), len(want.RefreshToken))
	}
	if got.DeviceID != want.DeviceID {
		t.Errorf("DeviceID = %q, want %q", got.DeviceID, want.DeviceID)
	}
	if got.UserName != want.UserName {
		t.Errorf("UserName = %q, want %q", got.UserName, want.UserName)
	}
	if !got.SavedAt.Equal(want.SavedAt) {
		t.Errorf("SavedAt = %v, want %v", got.SavedAt, want.SavedAt)
	}
	if got.SavedAt.Location() != time.UTC {
		t.Errorf("SavedAt's location = %v, want UTC", got.SavedAt.Location())
	}
}

func mustPut(t *testing.T, s Store, origin string, c Credential) {
	t.Helper()
	if err := s.Put(origin, c); err != nil {
		t.Fatalf("Put(%q): %v", origin, err)
	}
}

func mustGet(t *testing.T, s Store, origin string) Credential {
	t.Helper()
	c, err := s.Get(origin)
	if err != nil {
		t.Fatalf("Get(%q): %v", origin, err)
	}
	return c
}

func mustList(t *testing.T, s Store) []string {
	t.Helper()
	origins, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return origins
}

func assertList(t *testing.T, s Store, want ...string) {
	t.Helper()
	if got := mustList(t, s); !slices.Equal(got, want) {
		t.Errorf("List = %q, want %q", got, want)
	}
}

// tokenForBlobSize returns a refresh token that makes sample's Credential Manager blob exactly n bytes.
func tokenForBlobSize(t *testing.T, n int) string {
	t.Helper()
	overhead := len(marshalBlob(sample(""), false))
	if n <= overhead {
		t.Fatalf("a blob of %d bytes can't hold a token (overhead %d)", n, overhead)
	}
	token := strings.Repeat("a", n-overhead)
	if got := len(marshalBlob(sample(token), false)); got != n {
		t.Fatalf("blob is %d bytes, want %d", got, n)
	}
	return token
}

// testStoreContract checks what every Store does (the package documentation). open returns an empty store and
// removes what the test left in it.
func testStoreContract(t *testing.T, open func(t *testing.T) Store) {
	t.Run("missing", func(t *testing.T) {
		s := open(t)
		if _, err := s.Get(originA); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get of a missing origin: err = %v, want ErrNotFound", err)
		}
		if err := s.Delete(originA); err != nil {
			t.Errorf("Delete of a missing origin: %v, want nil", err)
		}
		assertList(t, s)
	})

	t.Run("put and get", func(t *testing.T) {
		s := open(t)
		in := sample("isr_test-put-get")
		// A local time with nanoseconds and a monotonic reading comes back in UTC, to the millisecond.
		in.SavedAt = time.Now().In(time.FixedZone("UTC+9", 9*3600))
		mustPut(t, s, originA, in)
		assertCredential(t, mustGet(t, s, originA), in)
		assertList(t, s, originA)
	})

	t.Run("overwrite", func(t *testing.T) {
		s := open(t)
		mustPut(t, s, originA, sample("isr_test-first"))
		second := Credential{RefreshToken: "isr_test-second", DeviceID: "b8f2n4r6t0vz", UserName: "Zoë 🎮"}
		mustPut(t, s, originA, second) // a zero SavedAt and a non-ASCII user name round-trip too
		assertCredential(t, mustGet(t, s, originA), second)
		assertList(t, s, originA)
	})

	t.Run("delete", func(t *testing.T) {
		s := open(t)
		mustPut(t, s, originA, sample("isr_test-a"))
		mustPut(t, s, originB, sample("isr_test-b"))
		if err := s.Delete(originA); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, err := s.Get(originA); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get after Delete: err = %v, want ErrNotFound", err)
		}
		if err := s.Delete(originA); err != nil {
			t.Errorf("second Delete: %v, want nil", err)
		}
		assertCredential(t, mustGet(t, s, originB), sample("isr_test-b"))
		assertList(t, s, originB)
	})

	t.Run("list", func(t *testing.T) {
		s := open(t)
		for _, o := range []string{originC, originA, originB} {
			mustPut(t, s, o, sample("isr_test-list"))
		}
		want := []string{originA, originB, originC}
		slices.Sort(want)
		assertList(t, s, want...)
	})

	t.Run("blob size", func(t *testing.T) {
		s := open(t)
		largest := sample(tokenForBlobSize(t, maxBlobBytes))
		mustPut(t, s, originA, largest)
		assertCredential(t, mustGet(t, s, originA), largest)

		err := s.Put(originA, sample(tokenForBlobSize(t, maxBlobBytes+1)))
		if !errors.Is(err, errBlobTooLarge) {
			t.Fatalf("Put of a %d-byte blob: err = %v, want errBlobTooLarge", maxBlobBytes+1, err)
		}
		if strings.Contains(err.Error(), "aaaa") {
			t.Errorf("the error carries the token: %v", err)
		}
		assertCredential(t, mustGet(t, s, originA), largest) // the refused Put changed nothing
		if err := s.Put(originB, sample(tokenForBlobSize(t, 3*maxBlobBytes))); !errors.Is(err, errBlobTooLarge) {
			t.Fatalf("Put of a large blob: err = %v, want errBlobTooLarge", err)
		}
		if _, err := s.Get(originB); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get after a refused Put: err = %v, want ErrNotFound", err)
		}
	})

	t.Run("user name length", func(t *testing.T) {
		s := open(t)
		c := sample("isr_test-user")
		c.UserName = strings.Repeat("u", maxUserNameLen)
		mustPut(t, s, originA, c)
		assertCredential(t, mustGet(t, s, originA), c)
		c.UserName += "u"
		if err := s.Put(originA, c); !errors.Is(err, errInvalid) {
			t.Errorf("Put with a %d-unit user name: err = %v, want errInvalid", len(c.UserName), err)
		}
	})

	t.Run("invalid arguments", func(t *testing.T) {
		s := open(t)
		for _, origin := range []string{"", "https://watch.example.com\x00evil"} {
			if err := s.Put(origin, sample("isr_test-invalid")); !errors.Is(err, errInvalid) {
				t.Errorf("Put(%q): err = %v, want errInvalid", origin, err)
			}
			if _, err := s.Get(origin); !errors.Is(err, errInvalid) {
				t.Errorf("Get(%q): err = %v, want errInvalid", origin, err)
			}
			if err := s.Delete(origin); !errors.Is(err, errInvalid) {
				t.Errorf("Delete(%q): err = %v, want errInvalid", origin, err)
			}
		}
		bad := map[string]Credential{
			"no refresh token":   sample(""),
			"NUL in user name":   {RefreshToken: "isr_test-invalid", UserName: "Al\x00ex"},
			"SavedAt after 9999": {RefreshToken: "isr_test-invalid", SavedAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)},
		}
		for name, c := range bad {
			if err := s.Put(originA, c); !errors.Is(err, errInvalid) {
				t.Errorf("Put with %s: err = %v, want errInvalid", name, err)
			}
		}
		assertList(t, s)
	})

	t.Run("concurrent", func(t *testing.T) {
		s := open(t)
		const workers, rounds = 8, 5
		var wg sync.WaitGroup
		errs := make(chan error, workers*rounds*3)
		for w := range workers {
			wg.Go(func() {
				origin := fmt.Sprintf("https://w%d.example.com", w)
				for r := range rounds {
					c := sample(fmt.Sprintf("isr_test-%d-%d", w, r))
					if err := s.Put(origin, c); err != nil {
						errs <- fmt.Errorf("Put(%s): %w", origin, err)
						return
					}
					got, err := s.Get(origin)
					if err != nil {
						errs <- fmt.Errorf("Get(%s): %w", origin, err)
						return
					}
					if got.RefreshToken != c.RefreshToken {
						errs <- fmt.Errorf("Get(%s) returned another round's credential", origin)
					}
					if _, err := s.List(); err != nil {
						errs <- fmt.Errorf("List: %w", err)
					}
				}
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
		if got := mustList(t, s); len(got) != workers {
			t.Errorf("List has %d origins, want %d: %q", len(got), workers, got)
		}
	})
}

func TestMemoryStore(t *testing.T) {
	testStoreContract(t, func(*testing.T) Store { return NewMemory() })
}

func TestMemoryStoresAreIndependent(t *testing.T) {
	a, b := NewMemory(), NewMemory()
	mustPut(t, a, originA, sample("isr_test-a"))
	if _, err := b.Get(originA); !errors.Is(err, ErrNotFound) {
		t.Errorf("another memory store sees the credential: err = %v", err)
	}
}

func TestOpenRefusesABadPrefix(t *testing.T) {
	for _, prefix := range []string{"isshoni:*:", "isshoni\x00:"} {
		if _, err := Open(Options{Prefix: prefix, Dir: t.TempDir(), Logger: discardLogger()}); !errors.Is(err, errInvalid) {
			t.Errorf("Open with prefix %q: err = %v, want errInvalid", prefix, err)
		}
	}
}

func TestBlobFormat(t *testing.T) {
	c := sample("isr_test-format")
	const want = `{"v":1,"refreshToken":"isr_test-format","deviceId":"m4q8x2c6v0bn","savedAt":"2026-11-02T19:05:01.004Z"}`
	if got := string(marshalBlob(c, false)); got != want {
		t.Errorf("Credential Manager blob:\n got %s\nwant %s", got, want)
	}
	const withUser = `{"v":1,"refreshToken":"isr_test-format","deviceId":"m4q8x2c6v0bn","savedAt":"2026-11-02T19:05:01.004Z","userName":"Alex"}`
	if got := string(marshalBlob(c, true)); got != withUser {
		t.Errorf("file store entry:\n got %s\nwant %s", got, withUser)
	}
	got, reason := unmarshalBlob([]byte(withUser))
	if reason != "" {
		t.Fatalf("unmarshalBlob: %s", reason)
	}
	assertCredential(t, got, c)
}

func TestUnreadableBlobs(t *testing.T) {
	const token = "isr_test-secret-value"
	//nolint:gosec // G101: made-up blobs around a made-up token, what the reasons must not repeat
	blobs := map[string]string{
		"not JSON":      `{"v":1,"refreshToken":"` + token + `"`,
		"empty":         ``,
		"JSON null":     `null`,
		"another v":     `{"v":2,"refreshToken":"` + token + `","deviceId":"d","savedAt":"2026-11-02T19:05:01.004Z"}`,
		"no v":          `{"refreshToken":"` + token + `","deviceId":"d","savedAt":"2026-11-02T19:05:01.004Z"}`,
		"no token":      `{"v":1,"deviceId":"d","savedAt":"2026-11-02T19:05:01.004Z"}`,
		"bad savedAt":   `{"v":1,"refreshToken":"` + token + `","deviceId":"d","savedAt":"yesterday"}`,
		"wrong v type":  `{"v":"1","refreshToken":"` + token + `","deviceId":"d","savedAt":"2026-11-02T19:05:01.004Z"}`,
		"array instead": `["` + token + `"]`,
	}
	for name, b := range blobs {
		c, reason := unmarshalBlob([]byte(b))
		if reason == "" {
			t.Errorf("%s: accepted", name)
			continue
		}
		if c != (Credential{}) {
			t.Errorf("%s: returned a credential with the reason %q", name, reason)
		}
		if strings.Contains(reason, token) {
			t.Errorf("%s: the reason carries the token: %q", name, reason)
		}
	}
}

func TestCredentialRedaction(t *testing.T) {
	const token, user = "isr_test-redact-me", "SecretUserName" //nolint:gosec // G101: made up, what the redaction must hide
	c := Credential{RefreshToken: token, DeviceID: "m4q8x2c6v0bn", UserName: user, SavedAt: sample("").SavedAt}

	var outputs []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
		outputs = append(outputs, fmt.Sprintf(verb, c), fmt.Sprintf(verb, &c))
	}
	outputs = append(outputs,
		fmt.Sprint(c), fmt.Sprintln([]Credential{c}), c.String(), c.GoString(),
		fmt.Sprintf("%+v", struct{ C Credential }{c}),
		fmt.Sprintf("%v", map[string]Credential{"k": c}),
	)
	var logBuf bytes.Buffer
	for _, h := range []slog.Handler{slog.NewJSONHandler(&logBuf, nil), slog.NewTextHandler(&logBuf, nil)} {
		slog.New(h).Info("saved", "credential", c, "ptr", &c)
	}
	outputs = append(outputs, logBuf.String())
	data, err := json.Marshal(map[string]any{"c": c, "p": &c})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	outputs = append(outputs, string(data))

	for _, out := range outputs {
		if strings.Contains(out, token) || strings.Contains(out, user) {
			t.Errorf("output carries a secret: %s", out)
		}
	}
	if s := c.String(); !strings.Contains(s, redacted) || !strings.Contains(s, "m4q8x2c6v0bn") {
		t.Errorf("String = %q, want the token redacted and the device id shown", s)
	}
	if !strings.Contains(logBuf.String(), `"device_id":"m4q8x2c6v0bn"`) {
		t.Errorf("the slog JSON output lacks the device id: %s", logBuf.String())
	}
	if s := (Credential{DeviceID: "d"}).String(); strings.Contains(s, redacted) {
		t.Errorf("an empty token prints as %q, want nothing", s)
	}
}

// logBuffer collects JSON log lines from several goroutines.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// records returns the logged records with the given level and message substring.
func (b *logBuffer) records(t *testing.T, level, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.Lines(b.String()) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", line, err)
		}
		if m["level"] == level && strings.Contains(fmt.Sprint(m["msg"]), msg) {
			out = append(out, m)
		}
	}
	return out
}

func newLogBuffer() (*slog.Logger, *logBuffer) {
	b := &logBuffer{}
	return slog.New(slog.NewJSONHandler(b, nil)), b
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
