//go:build windows && !isshoni_hw

package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"
	"golang.org/x/sys/windows/registry"

	"github.com/MoonWX/isshoni/internal/desktop/app"
)

// TestToastRegistryKeys is W8's registry half (03 §14.1, §22.2): after one toast with buttons, the three HKCU keys
// exist under the application's Options.Name and CustomActivator names the CLSID whose LocalServer32 starts the
// executable with %1. The toast is sent by a child process (Wails keeps one application per process, and the
// service reads its Options.Name), under a name of its own (io.isshoni.test.<random>), and the test deletes the keys,
// so that on the owner's PC it never points the installed app's activator at a test binary; TestAppOptions checks
// that the app's Options.Name is io.isshoni.desktop. Delivering the toast itself needs a desktop session, so a push
// that fails is reported, not failed: the keys are written before it.
func TestToastRegistryKeys(t *testing.T) {
	if defaultAction != notifications.DefaultActionIdentifier {
		t.Fatalf("defaultAction %q, Wails says %q", defaultAction, notifications.DefaultActionIdentifier)
	}
	name := randomName(t)
	var guid string
	defer func() {
		for _, k := range []string{
			`Software\Classes\AppUserModelId\` + name,
			`Software\` + name + `\NotificationCategories`,
			`Software\` + name,
		} {
			_ = registry.DeleteKey(registry.CURRENT_USER, k)
		}
		if guid != "" {
			_ = registry.DeleteKey(registry.CURRENT_USER, `Software\Classes\CLSID\`+guid+`\LocalServer32`)
			_ = registry.DeleteKey(registry.CURRENT_USER, `Software\Classes\CLSID\`+guid)
			_ = os.Remove(filepath.Join(os.TempDir(), name+guid+".png"))
		}
	}()
	out, err := childCmd(t, "toast", []string{idEnv + "=" + name}).CombinedOutput()
	if code := exitCode(t, err); code != 0 {
		t.Fatalf("the toast child exited %d: %s", code, out)
	}
	t.Logf("child: %s", out)

	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Classes\AppUserModelId\`+name, registry.QUERY_VALUE)
	if err != nil {
		t.Fatalf("the AppUserModelId key: %v", err)
	}
	guid, _, err = k.GetStringValue("CustomActivator")
	_ = k.Close()
	if err != nil || !strings.HasPrefix(guid, "{") {
		t.Fatalf("CustomActivator %q, %v", guid, err)
	}
	k, err = registry.OpenKey(registry.CURRENT_USER, `Software\Classes\CLSID\`+guid+`\LocalServer32`,
		registry.QUERY_VALUE)
	if err != nil {
		t.Fatalf("the CLSID key of CustomActivator: %v", err)
	}
	server, _, err := k.GetStringValue("")
	_ = k.Close()
	exe, _ := filepath.Abs(os.Args[0])
	if err != nil || !strings.EqualFold(server, `"`+exe+`" %1`) {
		t.Errorf("LocalServer32 %q, %v; want %q", server, err, `"`+exe+`" %1`)
	}
	k, err = registry.OpenKey(registry.CURRENT_USER, `Software\`+name+`\NotificationCategories`, registry.QUERY_VALUE)
	if err != nil {
		t.Fatalf("the NotificationCategories key: %v", err)
	}
	cats, _, err := k.GetStringValue("Categories")
	_ = k.Close()
	if err != nil || !strings.Contains(cats, toastCategory(app.ToastLockPaused)) {
		t.Errorf("Categories %q, %v", cats, err)
	}
}

// childToast is the toast child: an application named name, the toasts' service started, one toast with buttons (a
// push that fails without a desktop session is only reported), and the service stopped, which saves the categories.
func childToast(name string) int {
	_ = application.New(application.Options{Name: name, Logger: slog.New(slog.DiscardHandler)})
	n := newNotifier(slog.New(slog.DiscardHandler))
	if err := n.start(); err != nil {
		fmt.Println("start:", err)
		return 3
	}
	err := n.notify(app.Notification{ID: "t-1", Kind: app.ToastLockPaused, Title: "isshoni", Body: "test",
		Nonce: "00112233445566778899aabbccddeeff", Actions: []app.NotificationAction{
			{ID: app.ToastResume, Title: "Resume"}, {ID: app.ToastStop, Title: "Stop"}}})
	if err != nil {
		fmt.Println("the toast wasn't delivered (no desktop session?):", err)
	}
	n.stop()
	return 0
}
