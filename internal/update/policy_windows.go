//go:build windows

package update

import "golang.org/x/sys/windows/registry"

// The machine policy (06 §8.8): HKLM\SOFTWARE\Policies\isshoni\DisableUpdateChecks = 1 turns the updater off for every
// user of the PC; 03 then shows "Turned off by your administrator". The installer never writes HKLM; an
// administrator or Group Policy does.
const (
	policyKey   = `SOFTWARE\Policies\isshoni`
	policyValue = "DisableUpdateChecks"
)

// PolicyDisabled reports whether the machine policy turns the updater off (06 §8.8): the DWORD
// HKLM\SOFTWARE\Policies\isshoni\DisableUpdateChecks is 1. A missing key or value, or another value, is no policy.
func PolicyDisabled() bool { return policyDisabled(registry.LOCAL_MACHINE, policyKey) }

// policyDisabled reads the policy value under root\path; the tests pass a key under HKCU.
func policyDisabled(root registry.Key, path string) bool {
	k, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer func() { _ = k.Close() }()
	v, typ, err := k.GetIntegerValue(policyValue)
	return err == nil && typ == registry.DWORD && v == 1
}
