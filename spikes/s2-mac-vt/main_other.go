//go:build !darwin

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "vtbench needs macOS (VideoToolbox)")
	os.Exit(1)
}
