//go:build (windows || (darwin && cgo)) && !isshoni_hw

package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/MoonWX/isshoni/internal/desktop/bridge"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/native"
)

// WA84: the OpenH264 opt-in over WA51's helpers (04 §9.9, §12.5) and the engine's SetOpenH264Path.

// h264Engine records SetOpenH264Path.
type h264Engine struct {
	media.Engine // nil: only SetOpenH264Path is called
	paths        []string
	err          error
}

func (e *h264Engine) SetOpenH264Path(path string) error {
	e.paths = append(e.paths, path)
	if path != "" {
		return e.err
	}
	return nil
}

// newTestOpenH264 is the opt-in over a temporary data root whose install writes a stand-in file (the real download
// and its checks are WA51's TestInstallOpenH264*), with WA51's real RemoveOpenH264 and a verify that checks the
// stand-in's bytes.
func newTestOpenH264(t *testing.T, eng *h264Engine) (*openH264, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	o := newOpenH264(eng, t.TempDir(), slog.New(slog.NewTextHandler(&logs, nil)))
	if o == nil {
		t.Fatal("no opt-in for an engine with SetOpenH264Path")
	}
	o.install = func(_ context.Context, dir string) (string, error) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
		p := filepath.Join(dir, native.OpenH264.FileName)
		return p, os.WriteFile(p, []byte("stand-in"), 0o600)
	}
	o.verify = func(path string) error {
		b, err := os.ReadFile(path) //nolint:gosec // G304: the test's own file
		if err != nil || string(b) != "stand-in" {
			return &media.Error{Code: media.CodeDependency, Reason: "missing", Op: "verify_openh264"}
		}
		return nil
	}
	return o, &logs
}

// TestOpenH264Codec: off, Enable installs and hands the file to the engine (on), a restart hands it again, Disable
// takes it from the engine first and deletes it; an engine that refuses the file leaves nothing behind.
func TestOpenH264Codec(t *testing.T) {
	var _ bridge.OpenH264Codec = (*openH264)(nil)
	eng := &h264Engine{}
	o, logs := newTestOpenH264(t, eng)
	if o.Version() != native.OpenH264.Version || o.Installed() {
		t.Fatalf("fresh: %s %v", o.Version(), o.Installed())
	}
	o.applyAtStart() // nothing installed: the engine isn't told
	if err := o.Enable(context.Background()); err != nil || !o.Installed() {
		t.Fatalf("Enable: %v, installed %v", err, o.Installed())
	}
	o.applyAtStart()
	if err := o.Disable(); err != nil || o.Installed() {
		t.Fatalf("Disable: %v, installed %v", err, o.Installed())
	}
	want := []string{o.path(), o.path(), ""}
	if !slices.Equal(eng.paths, want) {
		t.Fatalf("SetOpenH264Path calls %q, want %q", eng.paths, want)
	}

	eng.err = &media.Error{Code: media.CodeDependency, Reason: "version", Op: "set_openh264_path",
		Detail: filepath.Join("C:", "Users", "someone")}
	err := o.Enable(context.Background())
	var me *media.Error
	if !errors.As(err, &me) || me.Reason != "version" {
		t.Fatalf("a refused file: %v", err)
	}
	if o.Installed() {
		t.Fatal("a refused file was left behind")
	}
	if entries, _ := os.ReadDir(o.dir); len(entries) != 0 {
		t.Fatalf("left in the codecs folder: %v", entries)
	}
	if strings.Contains(logs.String(), "someone") {
		t.Fatal("an error's detail was logged")
	}
}

// TestOpenH264Unavailable: no engine, or one without SetOpenH264Path (the fake of dev and e2e builds): no opt-in.
func TestOpenH264Unavailable(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	if newOpenH264(nil, t.TempDir(), log) != nil {
		t.Error("an opt-in without an engine")
	}
	var plain struct{ media.Engine }
	if newOpenH264(plain, t.TempDir(), log) != nil {
		t.Error("an opt-in for an engine that can't take it")
	}
	var o *openH264
	o.applyAtStart() // nil: nothing
}
