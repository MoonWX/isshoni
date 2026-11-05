//go:build windows || (darwin && cgo)

package main

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"

	"github.com/MoonWX/isshoni/internal/desktop/bridge"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/native"
)

// The Cisco OpenH264 opt-in (03 §14.1, §14.4; 04 §9.9, §12.5; README WA84): Settings → Sharing's Enable downloads
// Cisco's pinned binary into %LOCALAPPDATA%\isshoni\codecs through WA51's helpers, which check its size and SHA-256,
// and hands it to the engine; Turn off and delete takes it away from the engine and deletes it. The file itself is
// the setting: at every start a file that still verifies goes to the engine again.

// codecsDir is the opt-in's folder in the data root (03 §14.1).
const codecsDir = "codecs"

// openH264Engine is the engine's part of the opt-in (native.Engine's SetOpenH264Path, found by a type assertion: the
// fake engine of dev and e2e builds has none, so they don't offer it).
type openH264Engine interface {
	SetOpenH264Path(path string) error
}

var _ openH264Engine = (*native.Engine)(nil)

// openH264 is bridge.OpenH264Codec over the engine and the codecs folder. Enable and Disable run one at a time.
type openH264 struct {
	eng openH264Engine
	dir string
	log *slog.Logger

	// The helpers, replaced by the tests.
	install func(ctx context.Context, dir string) (string, error)
	verify  func(path string) error
	remove  func(dir string) error

	mu sync.Mutex
}

var _ bridge.OpenH264Codec = (*openH264)(nil)

// newOpenH264 is the opt-in for engine in the data root, nil when the engine can't take OpenH264 (none opened, or
// the fake): the page then doesn't offer it.
func newOpenH264(engine media.Engine, root string, log *slog.Logger) *openH264 {
	eng, ok := engine.(openH264Engine)
	if engine == nil || !ok || root == "" {
		return nil
	}
	return &openH264{
		eng: eng,
		dir: filepath.Join(root, codecsDir),
		log: log.With("component", "openh264"),
		install: func(ctx context.Context, dir string) (string, error) {
			return native.InstallOpenH264(ctx, nil, dir)
		},
		verify: native.VerifyOpenH264,
		remove: native.RemoveOpenH264,
	}
}

// path is the pinned file in the codecs folder.
func (o *openH264) path() string { return filepath.Join(o.dir, native.OpenH264.FileName) }

// Version is the pinned release.
func (o *openH264) Version() string { return native.OpenH264.Version }

// Installed reports the pinned file, verified (size, then SHA-256).
func (o *openH264) Installed() bool { return o.verify(o.path()) == nil }

// Enable downloads and verifies Cisco's binary (04 §9.9 steps 1–4) and hands it to the engine, which checks its
// version. When the engine refuses it, the file is removed again: a failure leaves nothing behind.
func (o *openH264) Enable(ctx context.Context) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	path, err := o.install(ctx, o.dir)
	if err != nil {
		o.log.Warn("desktop: OpenH264 wasn't installed", "err", redactedMedia(err))
		return err
	}
	if err := o.eng.SetOpenH264Path(path); err != nil {
		o.log.Warn("desktop: the engine refused OpenH264", "err", redactedMedia(err))
		if rerr := o.remove(o.dir); rerr != nil {
			o.log.Warn("desktop: the refused OpenH264 wasn't removed", "err", redactedMedia(rerr))
		}
		return err
	}
	o.log.Info("desktop: OpenH264 is on", "version", native.OpenH264.Version)
	return nil
}

// Disable takes OpenH264 away from the engine, then deletes the file (the engine must have let go of the DLL first).
func (o *openH264) Disable() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.eng.SetOpenH264Path(""); err != nil {
		o.log.Warn("desktop: the engine didn't turn OpenH264 off", "err", redactedMedia(err))
		return err
	}
	if err := o.remove(o.dir); err != nil {
		o.log.Warn("desktop: OpenH264 wasn't deleted", "err", redactedMedia(err))
		return err
	}
	o.log.Info("desktop: OpenH264 is off and deleted")
	return nil
}

// applyAtStart hands a file that still verifies to the engine at start (the opt-in survives a restart); a file that
// doesn't verify any more is left for the user's Turn off (and is never loaded).
func (o *openH264) applyAtStart() {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.verify(o.path()); err != nil {
		var me *media.Error
		if errors.As(err, &me) && me.Reason == "missing" {
			return // not turned on
		}
		o.log.Warn("desktop: the installed OpenH264 doesn't verify; not used", "err", redactedMedia(err))
		return
	}
	if err := o.eng.SetOpenH264Path(o.path()); err != nil {
		o.log.Warn("desktop: the engine refused the installed OpenH264", "err", redactedMedia(err))
	}
}

// redactedMedia is an error for the log: a media error's Redacted form (op, code and reason, never its detail, which
// may hold a path with the user's name), else the error's kind only.
func redactedMedia(err error) string {
	var me *media.Error
	if errors.As(err, &me) {
		return me.Redacted()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err.Error()
	}
	return "error"
}
