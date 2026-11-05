package winos

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
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

// fakeStore is a toastStore in memory; afterFirstList runs once, after the first list (a toast delivered or dismissed
// meanwhile), and the errors fail the step they name.
type fakeStore struct {
	items          []deliveredItem
	lists          int
	afterFirstList func(f *fakeStore)
	clearErr       error
	showErr        error
	removed        []string
	cleared        int
	shown          []Toast
}

func (f *fakeStore) list() ([]deliveredItem, error) {
	out := slices.Clone(f.items)
	f.lists++
	if f.lists == 1 && f.afterFirstList != nil {
		f.afterFirstList(f)
	}
	return out, nil
}

func (f *fakeStore) remove(tag, group string) error {
	f.removed = append(f.removed, tag+"/"+group)
	f.items = slices.DeleteFunc(f.items, func(it deliveredItem) bool { return it.Tag == tag && it.Group == group })
	return nil
}

func (f *fakeStore) clear() error {
	if f.clearErr != nil {
		return f.clearErr
	}
	f.cleared++
	f.items = nil
	return nil
}

func (f *fakeStore) show(t Toast) error {
	if f.showErr != nil {
		return f.showErr
	}
	f.shown = append(f.shown, t)
	f.items = append(f.items, deliveredItem{DeliveredToast: DeliveredToast{Tag: t.Tag, Group: t.Group, ID: t.XML}, xml: t.XML})
	return nil
}

// item is a delivered toast whose ID is also its XML (enough for the fake).
func item(id, tag string) deliveredItem {
	return deliveredItem{DeliveredToast: DeliveredToast{Tag: tag, ID: id}, xml: id}
}

func matchIDs(ids ...string) func(DeliveredToast) bool {
	return func(t DeliveredToast) bool { return slices.Contains(ids, t.ID) }
}

// TestRemoveDeliveredTagged: tagged matches go one by one; nothing is cleared.
func TestRemoveDeliveredTagged(t *testing.T) {
	f := &fakeStore{items: []deliveredItem{item("a", "ta"), item("b", ""), item("c", "tc")}}
	n, err := removeDelivered(f, matchIDs("a", "c"))
	if n != 2 || err != nil || f.cleared != 0 || !slices.Equal(f.removed, []string{"ta/", "tc/"}) {
		t.Fatalf("%d %v cleared %d removed %v", n, err, f.cleared, f.removed)
	}
}

// TestRemoveDeliveredUntagged: an untagged match clears the app's toasts and delivers the rest again, silently and
// with a tag of their own; a toast delivered after the first list is kept, and one dismissed meanwhile isn't brought
// back.
func TestRemoveDeliveredUntagged(t *testing.T) {
	f := &fakeStore{items: []deliveredItem{item("a", ""), item("b", ""), item("gone", "")}}
	f.afterFirstList = func(f *fakeStore) {
		f.items = slices.DeleteFunc(f.items, func(it deliveredItem) bool { return it.ID == "gone" })
		f.items = append(f.items, item("late", "")) // the shell's next toast, landing during the removal
	}
	n, err := removeDelivered(f, matchIDs("a"))
	if n != 1 || err != nil || f.cleared != 1 {
		t.Fatalf("%d %v cleared %d", n, err, f.cleared)
	}
	var ids []string
	for _, s := range f.shown {
		if !s.SuppressPopup || s.Tag != keepTag(s.XML) {
			t.Errorf("delivered again as %+v", s)
		}
		ids = append(ids, s.XML)
	}
	if !slices.Equal(ids, []string{"b", "late"}) {
		t.Fatalf("delivered again: %v", ids)
	}
}

// TestRemoveDeliveredDismissedMeanwhile: when the untagged match is gone by the second list, nothing is cleared.
func TestRemoveDeliveredDismissedMeanwhile(t *testing.T) {
	f := &fakeStore{items: []deliveredItem{item("a", ""), item("b", "")}}
	f.afterFirstList = func(f *fakeStore) { f.items = f.items[1:] }
	if n, err := removeDelivered(f, matchIDs("a")); n != 0 || err != nil || f.cleared != 0 || len(f.shown) != 0 {
		t.Fatalf("%d %v cleared %d shown %v", n, err, f.cleared, f.shown)
	}
}

// TestRemoveDeliveredCounts: a failed clear counts no untagged toast; a failed delivery says how many kept toasts
// were lost.
func TestRemoveDeliveredCounts(t *testing.T) {
	boom := errors.New("boom")
	f := &fakeStore{items: []deliveredItem{item("a", "ta"), item("b", ""), item("c", "")}, clearErr: boom}
	n, err := removeDelivered(f, matchIDs("a", "b"))
	if n != 1 || !errors.Is(err, boom) {
		t.Fatalf("clear fails: %d %v", n, err)
	}
	f = &fakeStore{items: []deliveredItem{item("b", ""), item("c", ""), item("d", "")}, showErr: boom}
	n, err = removeDelivered(f, matchIDs("b"))
	if n != 1 || !errors.Is(err, boom) || !strings.Contains(err.Error(), "2 of 2 kept toasts lost") {
		t.Fatalf("show fails: %d %v", n, err)
	}
}
