package winos

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// wailsLaunchOf is a launch attribute as Wails' Windows notifier writes it at beta.34 (encodePayload: base64 of
// {"action", "payload": NotificationOptions}); internal/desktop doesn't import Wails (03 D2), so the shape is
// written out here.
func wailsLaunchOf(t *testing.T, action, id, nonce string) string {
	t.Helper()
	opts := map[string]any{"id": id, "title": "isshoni", "body": "b"}
	if nonce != "" {
		opts["data"] = map[string]any{"nonce": nonce}
	}
	b, err := json.Marshal(map[string]any{"action": action, "payload": opts})
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// TestDecodeWailsLaunch: a toast's app.Notification ID and nonce come from Wails' launch payload; anything else
// gives none.
func TestDecodeWailsLaunch(t *testing.T) {
	const nonce = "00112233445566778899aabbccddeeff"
	id, n := decodeWailsLaunch(wailsLaunchOf(t, "DEFAULT_ACTION", "lock-paused-3", nonce))
	if id != "lock-paused-3" || n != nonce {
		t.Fatalf("id %q nonce %q", id, n)
	}
	if id, n := decodeWailsLaunch(wailsLaunchOf(t, "DEFAULT_ACTION", "still-sharing-1", "")); id != "still-sharing-1" ||
		n != "" {
		t.Fatalf("without a nonce: id %q nonce %q", id, n)
	}
	for _, launch := range []string{"", "not base64!", base64.StdEncoding.EncodeToString([]byte("[1]")),
		base64.StdEncoding.EncodeToString([]byte(`{"payload":{"id":7}}`))} {
		if id, n := decodeWailsLaunch(launch); id != "" || n != "" {
			t.Errorf("%q: id %q nonce %q", launch, id, n)
		}
	}
	// A nonce that isn't a string is none.
	b, _ := json.Marshal(map[string]any{"action": "x", "payload": map[string]any{"id": "a-1", "data": map[string]any{
		"nonce": 5}}})
	if id, n := decodeWailsLaunch(base64.StdEncoding.EncodeToString(b)); id != "a-1" || n != "" {
		t.Fatalf("a numeric nonce: id %q nonce %q", id, n)
	}
}

// TestDeliveredToast reads the launch attribute out of a toast's XML as Action Center returns it.
func TestDeliveredToast(t *testing.T) {
	launch := wailsLaunchOf(t, "DEFAULT_ACTION", "stopped-locked-4", "ffeeddccbbaa99887766554433221100")
	doc := `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<toast activationType="foreground" launch="` + launch + `" duration="short"><visual>` +
		`<binding template="ToastGeneric"><text>isshoni</text><text>Sharing stopped</text></binding></visual>` +
		`<actions><action content="Open" arguments="x" activationType="foreground"/></actions></toast>`
	got := deliveredToast("", "g", doc)
	want := DeliveredToast{Group: "g", Launch: launch, ID: "stopped-locked-4", Nonce: "ffeeddccbbaa99887766554433221100"}
	if got != want {
		t.Fatalf("%+v, want %+v", got, want)
	}
	for _, doc := range []string{"", "<tile/>", "<toast>", `<toast launch="plain"/>`} {
		if got := deliveredToast("t", "", doc); got.ID != "" || got.Tag != "t" {
			t.Errorf("%q: %+v", doc, got)
		}
	}
	if got := toastLaunch(`<toast launch="plain"/>`); got != "plain" {
		t.Fatalf("launch %q", got)
	}
}

// TestRemoveToastsNone: no ids removes nothing and touches no Action Center.
func TestRemoveToastsNone(t *testing.T) {
	var h Hooks
	if n, err := h.RemoveToasts(); n != 0 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
}
