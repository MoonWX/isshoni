//go:build (windows || (darwin && cgo)) && e2e

package main

func init() { flavor.e2e = true }
