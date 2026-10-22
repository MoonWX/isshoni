//go:build (windows || (darwin && cgo)) && dev

package main

import "os"

// devServerEnv names the Vite dev server for the asset handler (`task desktop:dev` sets it; 03 §5.3). Only dev
// builds read it.
const devServerEnv = "ISSHONI_DESKTOP_DEV_URL"

func init() {
	flavor.dev = true
	flavor.devServer = func() string { return os.Getenv(devServerEnv) }
}
