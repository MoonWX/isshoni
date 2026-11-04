package winos

import (
	"errors"
	"testing"

	"github.com/MoonWX/isshoni/internal/desktop/app"
)

// TestSettingsTarget (03 §22.2): every page's URI by build, sound-communications as control.exe with exactly
// "mmsys.cpl,,3", and nothing else opens.
func TestSettingsTarget(t *testing.T) {
	cases := []struct {
		page   app.OSPage
		build  int
		file   string
		params string
	}{
		{app.PageNotifications, 26100, "ms-settings:notifications", ""},
		{app.PageDND, 22000, "ms-settings:notifications", ""},
		{app.PageDND, 26100, "ms-settings:notifications", ""},
		{app.PageDND, 19045, "ms-settings:quiethours", ""},
		{app.PageGraphicsCapture, 26100, "ms-settings:privacy-graphicsCaptureProgrammatic", ""},
		{app.PageSound, 19045, "ms-settings:sound", ""},
		{app.PageSoundCommunications, 26100, "control.exe", "mmsys.cpl,,3"},
		{app.PageTaskbar, 26100, "ms-settings:taskbar", ""},
	}
	for _, c := range cases {
		file, params, err := settingsTarget(c.page, c.build)
		if err != nil || file != c.file || params != c.params {
			t.Errorf("%s on %d: %q %q %v, want %q %q", c.page, c.build, file, params, err, c.file, c.params)
		}
	}
	for _, p := range []app.OSPage{"", "display", "ms-settings:display", "search-ms:query=x"} {
		if _, _, err := settingsTarget(p, 26100); !errors.Is(err, app.ErrInvalidArgument) {
			t.Errorf("%q: %v", p, err)
		}
	}
}
