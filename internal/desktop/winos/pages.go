package winos

import (
	"fmt"

	"github.com/MoonWX/isshoni/internal/desktop/app"
)

// build22000 is Windows 11's first build: Do not disturb lives on the Notifications page there, Focus assist on its
// own page before (03 §11.7).
const build22000 = 22000

// settingsTarget is what OpenSettingsPage runs for p on Windows build `build` (03 §6.2 App.OpenOSSettings, §10.5,
// §11.7): ShellExecuteW's file and its parameters. Every page but sound-communications is an ms-settings: URI;
// sound-communications is control.exe with exactly "mmsys.cpl,,3" (the Sound control panel's Communications tab,
// 05's ducking tip). A page outside the list is app.ErrInvalidArgument.
func settingsTarget(p app.OSPage, build int) (file, params string, err error) {
	switch p {
	case app.PageNotifications:
		return "ms-settings:notifications", "", nil
	case app.PageDND:
		if build >= build22000 {
			return "ms-settings:notifications", "", nil
		}
		return "ms-settings:quiethours", "", nil
	case app.PageGraphicsCapture:
		return "ms-settings:privacy-graphicsCaptureProgrammatic", "", nil
	case app.PageSound:
		return "ms-settings:sound", "", nil
	case app.PageSoundCommunications:
		return "control.exe", "mmsys.cpl,,3", nil
	case app.PageTaskbar:
		return "ms-settings:taskbar", "", nil
	}
	return "", "", fmt.Errorf("%w: not a settings page", app.ErrInvalidArgument)
}
