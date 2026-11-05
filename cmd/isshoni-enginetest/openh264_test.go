package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/native"
)

func runOpenH264Args(t *testing.T, h openH264Helpers, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	g := &globals{stdout: &out, stderr: &errOut, json: true, open: native.Open}
	code := runOpenH264(context.Background(), g, args, h)
	return code, out.String(), errOut.String()
}

// TestOpenH264Command: each action calls its helper with -dir, prints the pin, and maps the helper's errors to the
// exit codes (corrupt 1, a download or a missing file 2).
func TestOpenH264Command(t *testing.T) {
	dir := t.TempDir()
	var calls []string
	h := openH264Helpers{
		install: func(_ context.Context, c *http.Client, d string) (string, error) {
			calls = append(calls, "install "+d)
			if c != nil {
				t.Error("the tool passes no client: the binding makes its own")
			}
			return filepath.Join(d, native.OpenH264.FileName), nil
		},
		verify: func(p string) error {
			calls = append(calls, "verify "+p)
			return nil
		},
		remove: func(d string) error {
			calls = append(calls, "remove "+d)
			return nil
		},
	}
	for _, action := range []string{"install", "verify", "remove"} {
		code, out, stderr := runOpenH264Args(t, h, action, "-dir", dir)
		if code != exitPass {
			t.Fatalf("%s: exit %d\n%s", action, code, stderr)
		}
		var r openH264Result
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatalf("%s: %v\n%s", action, err, out)
		}
		if !r.OK || r.Action != action || r.Version != native.OpenH264.Version || r.SHA256 != native.OpenH264.SHA256 ||
			r.Size != native.OpenH264.Size {
			t.Errorf("%s: %+v", action, r)
		}
	}
	want := []string{"install " + dir, "verify " + filepath.Join(dir, native.OpenH264.FileName), "remove " + dir}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls %q, want %q", calls, want)
	}

	for _, tc := range []struct {
		err  *media.Error
		code int
	}{
		{&media.Error{Code: media.CodeDependency, Op: "install_openh264", Reason: "corrupt"}, exitFail},
		{&media.Error{Code: media.CodeDependency, Op: "install_openh264", Reason: "download"}, exitEnv},
		{&media.Error{Code: media.CodeTimeout, Op: "install_openh264"}, exitEnv},
		{&media.Error{Code: media.CodeDependency, Op: "verify_openh264", Reason: "missing"}, exitEnv},
		{&media.Error{Code: media.CodeInternal, Op: "remove_openh264"}, exitFail},
	} {
		failing := openH264Helpers{
			install: func(context.Context, *http.Client, string) (string, error) { return "", tc.err },
			verify:  func(string) error { return tc.err },
			remove:  func(string) error { return tc.err },
		}
		action := map[string]string{"install_openh264": "install", "verify_openh264": "verify",
			"remove_openh264": "remove"}[tc.err.Op]
		code, out, _ := runOpenH264Args(t, failing, action, "-dir", dir)
		if code != tc.code {
			t.Errorf("%v: exit %d, want %d", tc.err, code, tc.code)
		}
		if !strings.Contains(out, `"ok": false`) {
			t.Errorf("%v: output %s", tc.err, out)
		}
	}
}

// TestOpenH264CommandReal: the real helpers on a folder: verify without a file is the environment, remove of an
// empty folder passes.
func TestOpenH264CommandReal(t *testing.T) {
	dir := t.TempDir()
	if code, _, stderr := runOpenH264Args(t, nativeOpenH264, "verify", "-dir", dir); code != exitEnv {
		t.Errorf("verify of an empty folder: exit %d, want %d\n%s", code, exitEnv, stderr)
	}
	if err := os.WriteFile(filepath.Join(dir, native.OpenH264.FileName), []byte("not the DLL"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := runOpenH264Args(t, nativeOpenH264, "verify", "-dir", dir); code != exitFail {
		t.Errorf("verify of a wrong file: exit %d, want %d", code, exitFail)
	}
	if code, _, stderr := runOpenH264Args(t, nativeOpenH264, "remove", "-dir", dir); code != exitPass {
		t.Errorf("remove: exit %d\n%s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, native.OpenH264.FileName)); !os.IsNotExist(err) {
		t.Errorf("the file is still there: %v", err)
	}
}

func TestOpenH264Usage(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
	}{
		{nil, exitUsage},
		{[]string{"download"}, exitUsage},
		{[]string{"-h"}, exitPass},
		{[]string{"install", "extra"}, exitUsage},
		{[]string{"install", "-engine"}, exitUsage},
		{[]string{"remove", "-dir", ""}, exitUsage},
	} {
		if code, _, stderr := runOpenH264Args(t, nativeOpenH264, tc.args...); code != tc.code {
			t.Errorf("%q: exit %d, want %d\n%s", tc.args, code, tc.code, stderr)
		}
	}
	// Registered with the tool's dispatch.
	if _, _, stderr := runArgs(t); !strings.Contains(stderr, "openh264") {
		t.Errorf("the usage text lists no openh264:\n%s", stderr)
	}
}
