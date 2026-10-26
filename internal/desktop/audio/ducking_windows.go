package audio

import (
	"errors"

	"golang.org/x/sys/windows/registry"
)

// duckingDoNothingValue is UserDuckingPreference's "Do nothing".
const duckingDoNothingValue = 3

// readDucking reads HKCU\Software\Microsoft\Multimedia\Audio\UserDuckingPreference (REG_DWORD; absent = Windows'
// default, which ducks). A value that can't be read is unknown, which hides the tip.
func readDucking() duckingPref {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Multimedia\Audio`, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return duckingReduces
	}
	if err != nil {
		return duckingUnknown
	}
	defer func() { _ = k.Close() }()
	v, _, err := k.GetIntegerValue("UserDuckingPreference")
	switch {
	case errors.Is(err, registry.ErrNotExist):
		return duckingReduces
	case err != nil:
		return duckingUnknown
	case v == duckingDoNothingValue:
		return duckingDoNothing
	}
	return duckingReduces
}
