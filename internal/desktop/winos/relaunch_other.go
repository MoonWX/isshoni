//go:build !windows

package winos

// RelaunchUnderShell does nothing off Windows (03 §13.4 step 2 is Windows' D16): false, nil.
func RelaunchUnderShell([]string) (bool, error) { return false, nil }

// ParentExe is "" off Windows: the relaunch looks at no parent there.
func ParentExe() (string, error) { return "", nil }
