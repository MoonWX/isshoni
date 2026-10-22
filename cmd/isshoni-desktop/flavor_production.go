//go:build (windows || (darwin && cgo)) && production

package main

func init() { flavor.production = true }
