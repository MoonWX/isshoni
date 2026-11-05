//go:build (windows || (darwin && cgo)) && !dev && !e2e

package main

import (
	"log/slog"
	"os"
	"runtime"

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

// soundCheckExe is audio.SelfInfo.Exe with the shipped engine: this exe, which 05's sound check starts with
// --sound-check-helper on Windows (05 §11.3); "" elsewhere, or when the path is unknown (the engine then plays the
// chime itself).
func soundCheckExe() string {
	if runtime.GOOS != "windows" {
		return ""
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return exe
}
