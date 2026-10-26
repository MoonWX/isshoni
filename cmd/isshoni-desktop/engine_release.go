//go:build (windows || (darwin && cgo)) && !dev && !e2e

package main

import (
	"log/slog"

	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/native"
)

// openEngine opens the Windows engine, isshoni-media.dll and its FFmpeg DLLs from the exe's folder (01 §7.1,
// 04 §12.2): every release build. On macOS (no engine before M3) it returns media.ErrUnsupportedOS's kind of error,
// and the app runs without sharing.
func openEngine(log *slog.Logger, debug bool) (media.Engine, error) {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	e, err := native.Open(native.Options{Logger: log, LogLevel: level})
	if err != nil {
		return nil, err
	}
	return e, nil
}
