//go:build darwin && cgo

package main

import (
	"fmt"
	"io"
	"os"
)

// restrictDLLSearch does nothing on macOS: 03 §13.4's first action is about Windows' DLL search order.
func restrictDLLSearch() error { return nil }

// versionOutput is stdout.
func versionOutput() io.Writer { return os.Stdout }

func firstActionError(err error) error { return fmt.Errorf("the first action failed: %w", err) }
