//go:build windows

package native

import "testing"

// The DLL-backed session tests (session_dll_test.go) run against ISSHONI_TEST_ENGINE_DLL, the win-x64-ci DLL (04
// §18.2): openDLL skips without it.
func init() {
	sessionEngine = func(t *testing.T) *Engine {
		t.Helper()
		return openDLL(t)
	}
}
