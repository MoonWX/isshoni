//go:build !windows

package main

import "errors"

func runWindowsCommand(string, []string) error {
	return errors.New("this command runs on Windows only (s1 analyze works everywhere)")
}
