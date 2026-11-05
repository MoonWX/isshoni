//go:build windows

package winos

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"slices"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"
)

// testAppID registers a random AppUserModelID for this test (HKCU\Software\Classes\AppUserModelId\<id> with a
// DisplayName, which lets an unpackaged process show toasts under it) and removes it, with its toasts, afterwards.
// The app's own io.isshoni.desktop is never touched: on the owner's PC its toasts are the installed app's.
func testAppID(t *testing.T) string {
	t.Helper()
	var b [6]byte
	_, _ = rand.Read(b[:])
	id := "io.isshoni.test." + hex.EncodeToString(b[:])
	path := `Software\Classes\AppUserModelId\` + id
	k, _, err := registry.CreateKey(registry.CURRENT_USER, path, registry.SET_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	err = k.SetStringValue("DisplayName", "isshoni test")
	_ = k.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := RemoveDeliveredToasts(id, func(DeliveredToast) bool { return true }); err != nil {
			t.Errorf("cleanup: %v", err)
		}
		if err := registry.DeleteKey(registry.CURRENT_USER, path); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	return id
}

// wailsToastLaunch is the launch attribute Wails writes for app.Notification id with nonce (toasts_test.go's
// wailsLaunchOf; internal/desktop doesn't import Wails, 03 D2).
func wailsToastLaunch(t *testing.T, id, nonce string) string {
	t.Helper()
	return wailsLaunchOf(t, "DEFAULT_ACTION", id, nonce)
}

func testToastXML(launch, body string) string {
	return `<toast activationType="foreground" launch="` + launch + `"><visual><binding template="ToastGeneric">` +
		`<text>isshoni test</text><text>` + body + `</text></binding></visual><audio silent="true"/></toast>`
}

// waitDelivered polls Action Center until appID has n toasts (its history may lag a Show a little).
func waitDelivered(t *testing.T, appID string, n int) []DeliveredToast {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := DeliveredToasts(appID)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d toasts in Action Center, want %d: %+v", len(got), n, got)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func idsOf(ts []DeliveredToast) []string {
	ids := make([]string, 0, len(ts))
	for _, t := range ts {
		ids = append(ids, t.ID)
	}
	slices.Sort(ids)
	return ids
}

// TestActionCenterRemovesStaleToasts is 03 §15's removal on Windows (README WA68 R3): three toasts as the shell
// shows them through Wails (an untagged toast whose launch is Wails' payload) plus a tagged one; removing two by
// their app.Notification IDs leaves the third in Action Center with its XML, now tagged, and removing that one
// takes it by its tag. Toasts that are off for this user (a policy, the user's setting) skip it with the reason. A
// fresh test AppUserModelID has no NotificationSetting until its first toast (errNoSetting), so the setting is read
// again after the toasts are shown, before Action Center is checked; it must be readable then.
func TestActionCenterRemovesStaleToasts(t *testing.T) {
	appID := testAppID(t)
	if s, err := notificationSetting(appID); err != nil && !errors.Is(err, errNoSetting) {
		t.Fatal(err)
	} else if err == nil && s != 0 {
		t.Skipf("toasts are off for this user or app (NotificationSetting %d)", s)
	}
	const nonceA, nonceB, nonceC = "0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210",
		"00000000000000000000000000000001"
	launchC := wailsToastLaunch(t, "stopped-locked-3", nonceC)
	for _, toast := range []Toast{
		{XML: testToastXML(wailsToastLaunch(t, "lock-paused-1", nonceA), "tagged"), Tag: "a", Group: "g",
			SuppressPopup: true},
		{XML: testToastXML(wailsToastLaunch(t, "lock-paused-2", nonceB), "untagged"), SuppressPopup: true},
		{XML: testToastXML(launchC, "kept"), SuppressPopup: true},
	} {
		if err := ShowToast(appID, toast); err != nil {
			t.Fatal(err)
		}
	}
	if s, err := notificationSetting(appID); err != nil {
		t.Fatalf("after three toasts: %v", err)
	} else if s != 0 {
		t.Skipf("toasts are off for this user or app (NotificationSetting %d)", s)
	}
	got := waitDelivered(t, appID, 3)
	if ids := idsOf(got); !slices.Equal(ids, []string{"lock-paused-1", "lock-paused-2", "stopped-locked-3"}) {
		t.Fatalf("ids %v", ids)
	}
	for _, d := range got {
		switch d.ID {
		case "lock-paused-1":
			if d.Tag != "a" || d.Group != "g" || d.Nonce != nonceA {
				t.Errorf("the tagged toast: %+v", d)
			}
		case "lock-paused-2":
			if d.Tag != "" || d.Nonce != nonceB {
				t.Errorf("an untagged toast: %+v", d)
			}
		}
	}

	stale := []string{"lock-paused-1", "lock-paused-2"}
	n, err := RemoveDeliveredToasts(appID, func(d DeliveredToast) bool { return slices.Contains(stale, d.ID) })
	if err != nil || n != 2 {
		t.Fatalf("removed %d: %v", n, err)
	}
	got = waitDelivered(t, appID, 1)
	if d := got[0]; d.ID != "stopped-locked-3" || d.Nonce != nonceC || d.Launch != launchC || d.Tag == "" {
		t.Fatalf("the toast kept: %+v", d)
	}

	// The kept toast has a tag of its own now: it goes on its own.
	n, err = RemoveDeliveredToasts(appID, func(d DeliveredToast) bool { return d.ID == "stopped-locked-3" })
	if err != nil || n != 1 {
		t.Fatalf("removed %d: %v", n, err)
	}
	waitDelivered(t, appID, 0)
	if n, err := RemoveDeliveredToasts(appID, func(DeliveredToast) bool { return true }); err != nil || n != 0 {
		t.Fatalf("an empty Action Center: %d %v", n, err)
	}
}
