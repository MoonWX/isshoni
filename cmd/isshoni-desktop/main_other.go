//go:build !windows && !(darwin && cgo)

package main

import (
	"fmt"
	"os"
)

// main is the stub of every OS but Windows and macOS with cgo (06 §5.2, D18), so ./... builds and vets everywhere,
// also GOOS=darwin with cgo off.
func main() {
	fmt.Fprintln(os.Stderr, "isshoni-desktop runs on Windows and macOS; Linux uses isshoni-agent")
	os.Exit(1)
}
