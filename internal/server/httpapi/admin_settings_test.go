package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/auth"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// defaultSettingsJSON is the settings object of a new server (03 §9's table), in the DTO's field order.
const defaultSettingsJSON = `{"serverName":"","registrationMode":"invite","inviteDefaultTtlHours":168,` +
	`"inviteDefaultMaxUses":10,"membersCanInvite":false,"maxParticipantsPerRoom":0,"maxSharesPerRoom":0,` +
	`"maxShareBitrateKbps":0,"transferAlertGb":0,"updateCheck":true,"minClientVersion":"","setupWizardDone":false}`

// TestSettingsOverREST is 03 §15's settings case: GET and PATCH /admin/settings; a bad value → 422 with fields; a
// pinned field → 409; OnChange fires; a registration-mode change raises an alert and a security event.
func TestSettingsOverREST(t *testing.T) {
	f := newAuthFixture(t)
	admin, cookie := f.setupAdmin("Alex")
	_, token := f.createInvite(cookie, `{}`)
	_, memberCookie := f.join(token, "Sam", clientIP(0))
	var changes []string
	cancel := f.db.Settings().OnChange(func(prev, next store.Settings) {
		changes = append(changes, fmt.Sprintf("%s/%d→%s/%d", prev.RegistrationMode, prev.MaxSharesPerRoom,
			next.RegistrationMode, next.MaxSharesPerRoom))
	})
	defer cancel()
	f.signal.takeNotes()
	f.alerts.take()

	// GET: {settings, defaults, locked}; nothing is pinned, and locked is a list all the same.
	rec := f.call(http.MethodGet, "/api/v1/admin/settings", "", with(cookie))
	if got, want := rec.Body.String(),
		`{"settings":`+defaultSettingsJSON+`,"defaults":`+defaultSettingsJSON+`,"locked":[]}`+"\n"; rec.Code != http.StatusOK || got != want {
		t.Fatalf("GET settings: status %d, body %s; want %s", rec.Code, got, want)
	}

	// PATCH is a merge: only the fields present change, and the answer has the same shape as GET.
	rec = f.patchSettings(cookie, `{"registrationMode": "approval", "maxSharesPerRoom": 4, "serverName": "  Alex's  server "}`)
	got := decodeBody[api.SettingsResponse](t, rec, http.StatusOK)
	want := apiSettings(f.db.Settings().Defaults())
	want.RegistrationMode, want.MaxSharesPerRoom, want.ServerName = api.RegistrationModeApproval, 4, "Alex's server"
	if got.Settings != want || got.Defaults != apiSettings(f.db.Settings().Defaults()) || len(got.Locked) != 0 {
		t.Errorf("PATCH settings = %+v, want the settings %+v", got, want)
	}
	if keys := fmt.Sprint(jsonKeys(t, rec.Body.Bytes())); keys != "[defaults locked settings]" {
		t.Errorf("PATCH body keys = %s", keys)
	}
	if again := decodeBody[api.SettingsResponse](t, f.call(http.MethodGet, "/api/v1/admin/settings", "", with(cookie)), http.StatusOK); again.Settings != want {
		t.Errorf("GET after PATCH = %+v", again.Settings)
	}
	// OnChange fired, once for the whole patch.
	if fmt.Sprint(changes) != "[invite/0→approval/4]" {
		t.Errorf("OnChange calls = %v, want one for the patch", changes)
	}
	// Every admin's SPA refetches the settings; membersCanInvite did not change, so nobody refetches me.
	if notes := f.signal.takeNotes(); fmt.Sprint(notes) != "[admins [admin.settings]]" {
		t.Errorf("Signal after a settings change = %v", notes)
	}
	// The registration-mode change raised the alert, by the admin and without a target, …
	alerts := f.alerts.take()
	if len(alerts) != 1 || alerts[0] != (auth.AdminAlert{Kind: auth.AlertRegistrationModeChanged, Actor: "Alex", At: f.clk.now()}) {
		t.Fatalf("alerts = %+v, want one registration_mode_changed by Alex", alerts)
	}
	// … and its audit row, written with the IP from ClientIP, is a security event.
	rows := f.auditOf("settings.changed")
	if len(rows) != 1 || rows[0].Actor != (store.Actor{Kind: store.ActorUser, UserID: store.UserID(admin.ID), Name: "Alex", IP: addrA}) ||
		rows[0].TargetKind != "settings" {
		t.Fatalf("settings.changed rows = %+v", rows)
	}
	detail, _ := json.Marshal(rows[0].Detail)
	if got, want := string(detail), `{"changes":{"maxSharesPerRoom":{"from":0,"to":4},`+
		`"registrationMode":{"from":"invite","to":"approval"},"serverName":{"from":"","to":"Alex's server"}}}`; got != want {
		t.Errorf("settings.changed detail = %s, want %s", got, want)
	}
	f.read(func(q *store.Q) error {
		events, err := q.SecurityEvents(f.clk.now().Add(-time.Hour), 0)
		found := false
		for _, e := range events {
			found = found || e.ID == rows[0].ID
		}
		if err == nil && !found {
			t.Errorf("the registration-mode change is no security event: %+v", events)
		}
		return err
	})
	// The public info follows at once.
	if info := decodeBody[api.Info](t, f.call(http.MethodGet, "/api/v1/info", ""), http.StatusOK); info.Registration != api.RegistrationModeApproval ||
		info.Server.Name != "Alex's server" {
		t.Errorf("info after the change: registration %q, server name %q", info.Registration, info.Server.Name)
	}

	// A change without the registration mode: no alert, and no security event.
	decodeBody[api.SettingsResponse](t, f.patchSettings(cookie, `{"transferAlertGb":500}`), http.StatusOK)
	if alerts := f.alerts.take(); len(alerts) != 0 {
		t.Errorf("a change without the registration mode raised %+v", alerts)
	}
	f.signal.takeNotes()

	// A bad value → 422 with a code per field, null included; nothing is written, not the good fields either.
	rec = f.patchSettings(cookie, `{"registrationMode":"open","maxShareBitrateKbps":100,"inviteDefaultMaxUses":null,`+
		`"updateCheck":"yes","maxSharesPerRoom":7}`)
	e := wantError(t, rec, http.StatusUnprocessableEntity, api.CodeValidationFailed)
	if got, want := fmt.Sprint(e.Fields),
		"map[inviteDefaultMaxUses:invalid maxShareBitrateKbps:out_of_range registrationMode:invalid updateCheck:invalid]"; got != want {
		t.Errorf("fields = %s, want %s", got, want)
	}
	if f.db.Settings().Get().MaxSharesPerRoom != 4 {
		t.Error("a refused patch changed a setting")
	}

	// A patch that changes nothing: 200, no audit row, no notification, no OnChange call. Unknown names are ignored.
	before := len(f.auditOf("settings.changed"))
	changes = nil
	for _, body := range []string{`{}`, `{"maxSharesPerRoom":4}`, `{"registrationMode":"approval","serverName":"Alex's server"}`,
		`{"nope":1,"locked":["x"]}`} {
		res := decodeBody[api.SettingsResponse](t, f.patchSettings(cookie, body), http.StatusOK)
		if res.Settings.MaxSharesPerRoom != 4 || res.Settings.RegistrationMode != api.RegistrationModeApproval {
			t.Errorf("a no-op patch %s answered %+v", body, res.Settings)
		}
	}
	if len(f.auditOf("settings.changed")) != before || len(f.signal.takeNotes()) != 0 || len(f.alerts.take()) != 0 || len(changes) != 0 {
		t.Errorf("a no-op patch wrote an audit row, notified, alerted or ran OnChange (%v)", changes)
	}

	// A body that is not one JSON object → 400.
	for _, body := range []string{``, `null`, `[]`, `"registrationMode"`, `{"maxSharesPerRoom":4} {}`, `{"maxSharesPerRoom":`} {
		wantError(t, f.patchSettings(cookie, body), http.StatusBadRequest, api.CodeBadRequest)
	}
	wantError(t, f.patchSettings(cookie, `{"serverName":"`+strings.Repeat("x", otherBodyLimit)+`"}`),
		http.StatusRequestEntityTooLarge, api.CodePayloadTooLarge)

	// A field is reset by sending its default.
	res := decodeBody[api.SettingsResponse](t, f.patchSettings(cookie, `{"maxSharesPerRoom":0,"serverName":""}`), http.StatusOK)
	if res.Settings.MaxSharesPerRoom != 0 || res.Settings.ServerName != "" {
		t.Errorf("settings after a reset = %+v", res.Settings)
	}

	// Admins only: a member gets 403, nobody without a session gets in, and neither changes anything.
	for _, method := range []string{http.MethodGet, http.MethodPatch} {
		body := ""
		if method == http.MethodPatch {
			body = `{"registrationMode":"closed"}`
		}
		wantError(t, f.call(method, "/api/v1/admin/settings", body, with(memberCookie)), http.StatusForbidden, api.CodeForbidden)
		wantError(t, f.call(method, "/api/v1/admin/settings", body), http.StatusUnauthorized, api.CodeUnauthenticated)
	}
	if got := f.db.Settings().Get().RegistrationMode; got != store.ModeApproval {
		t.Errorf("registrationMode = %q after refused patches", got)
	}
	if len(f.alerts.take()) != 0 {
		t.Error("a refused patch raised an alert")
	}
}

// TestSettingsPinnedOverREST: a field pinned by the config file is listed in locked and refused with 409
// setting_locked, also for the value in force, and before any value is checked (03 §9, S23's choices).
func TestSettingsPinnedOverREST(t *testing.T) {
	f := newAuthFixture(t)
	_, cookie := f.setupAdmin("Alex")
	// As 04 does before serving, for the TOML keys updates.release_check and registration.mode.
	if err := f.db.Settings().Pin("updateCheck", false); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Settings().Pin("registrationMode", "closed"); err != nil {
		t.Fatal(err)
	}
	f.signal.takeNotes()
	f.alerts.take()

	rec := f.call(http.MethodGet, "/api/v1/admin/settings", "", with(cookie))
	got := decodeBody[api.SettingsResponse](t, rec, http.StatusOK)
	if fmt.Sprint(got.Locked) != "[registrationMode updateCheck]" || got.Settings.UpdateCheck || !got.Defaults.UpdateCheck ||
		got.Settings.RegistrationMode != api.RegistrationModeClosed || got.Defaults.RegistrationMode != api.RegistrationModeInvite {
		t.Fatalf("settings with pins = %+v", got)
	}
	if !strings.HasSuffix(rec.Body.String(), `,"locked":["registrationMode","updateCheck"]}`+"\n") {
		t.Errorf("GET body = %s, want locked to list the pinned fields", rec.Body)
	}

	// Refused whatever value is sent: another one, the value in force, the default, a bad one, null.
	for _, value := range []string{`true`, `false`, `"yes"`, `null`} {
		rec := f.patchSettings(cookie, `{"updateCheck":`+value+`}`)
		wantError(t, rec, http.StatusConflict, api.CodeSettingLocked)
		if got, want := rec.Body.String(), `{"error":{"code":"setting_locked","params":{"field":"updateCheck"}}}`+"\n"; got != want {
			t.Errorf("409 body for updateCheck = %s: %s, want %s", value, got, want)
		}
	}
	// It wins over validation_failed, and nothing else in the patch is written. With two pinned fields the first in
	// the settings' order is named.
	rec = f.patchSettings(cookie, `{"maxSharesPerRoom":4,"updateCheck":false,"inviteDefaultMaxUses":-1}`)
	if e := wantError(t, rec, http.StatusConflict, api.CodeSettingLocked); fmt.Sprint(e.Params) != "map[field:updateCheck]" || len(e.Fields) != 0 {
		t.Errorf("a pinned field next to a bad value: %+v", e)
	}
	rec = f.patchSettings(cookie, `{"updateCheck":true,"registrationMode":"invite"}`)
	if e := wantError(t, rec, http.StatusConflict, api.CodeSettingLocked); fmt.Sprint(e.Params) != "map[field:registrationMode]" {
		t.Errorf("two pinned fields: params %v, want the first in field order", e.Params)
	}
	if set := f.db.Settings().Get(); set.MaxSharesPerRoom != 0 || set.UpdateCheck || set.RegistrationMode != store.ModeClosed {
		t.Errorf("settings after refused patches = %+v", set)
	}
	if len(f.auditOf("settings.changed")) != 0 || len(f.signal.takeNotes()) != 0 || len(f.alerts.take()) != 0 {
		t.Error("a refused patch wrote an audit row, notified or alerted")
	}

	// The other fields still change, and the pinned ones stay as config says.
	res := decodeBody[api.SettingsResponse](t, f.patchSettings(cookie, `{"maxSharesPerRoom":4}`), http.StatusOK)
	if res.Settings.MaxSharesPerRoom != 4 || res.Settings.UpdateCheck || fmt.Sprint(res.Locked) != "[registrationMode updateCheck]" {
		t.Errorf("PATCH of an unpinned field = %+v", res)
	}
	// The pinned mode is the one in force: the server is closed, whatever the stored value.
	wantError(t, f.call(http.MethodPost, "/api/v1/invites", `{}`, with(cookie)), http.StatusForbidden, api.CodeRegistrationClosed)
}

// TestAPISettingsMirrorsStore: apiSettings copies every field of store.Settings, under the same JSON name.
func TestAPISettingsMirrorsStore(t *testing.T) {
	in := store.Settings{
		ServerName:             "Alex's server",
		RegistrationMode:       store.ModeApproval,
		InviteDefaultTTLHours:  24,
		InviteDefaultMaxUses:   3,
		MembersCanInvite:       true,
		MaxParticipantsPerRoom: 12,
		MaxSharesPerRoom:       4,
		MaxShareBitrateKbps:    8000,
		TransferAlertGB:        500,
		UpdateCheck:            true,
		MinClientVersion:       "0.2.0",
		SetupWizardDone:        true,
	}
	// Every field of the fixture is set, so a field apiSettings forgets shows as a difference.
	v := reflect.ValueOf(in)
	for i := range v.NumField() {
		if v.Field(i).IsZero() {
			t.Fatalf("the fixture leaves store.Settings.%s at its zero value: set it", v.Type().Field(i).Name)
		}
	}
	want, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(apiSettings(in))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("apiSettings = %s, want the store's %s", got, want)
	}
}

// TestAccountRoutes: the routes of this slice sit behind the /api/v1 chain like every other: JSON 405 with Allow,
// the CSRF check and the Content-Type rule on their unsafe methods (03 §12.1).
func TestAccountRoutes(t *testing.T) {
	f := newAuthFixture(t)
	_, cookie := f.setupAdmin("Alex")
	invite, _ := f.createInvite(cookie, `{}`)

	for path, allow := range map[string]string{
		"/api/v1/auth/register":                       "POST",
		"/api/v1/auth/invite/check":                   "POST",
		"/api/v1/invites/" + invite.ID:                "DELETE",
		"/api/v1/admin/approvals/x/approve":           "POST",
		"/api/v1/admin/approvals/x/reject":            "POST",
		"/api/v1/admin/approvals/all/reject":          "POST",
		"/api/v1/admin/approvals/x/reject/more":       "",
		"/api/v1/admin/approvals/x":                   "",
		"/api/v1/invites/" + invite.ID + "/something": "",
	} {
		rec := f.call(http.MethodGet, path, "", with(cookie))
		if allow == "" {
			wantError(t, rec, http.StatusNotFound, api.CodeNotFound)
			continue
		}
		wantError(t, rec, http.StatusMethodNotAllowed, api.CodeMethodNotAllowed)
		if got := rec.Header().Get("Allow"); got != allow {
			t.Errorf("GET %s: Allow %q, want %q", path, got, allow)
		}
	}
	for path, allow := range map[string]string{
		"/api/v1/invites":         "GET, HEAD, POST",
		"/api/v1/admin/approvals": "GET, HEAD",
		"/api/v1/admin/settings":  "GET, HEAD, PATCH",
	} {
		rec := f.call(http.MethodPut, path, `{}`, with(cookie))
		wantError(t, rec, http.StatusMethodNotAllowed, api.CodeMethodNotAllowed)
		if got := rec.Header().Get("Allow"); got != allow {
			t.Errorf("PUT %s: Allow %q, want %q", path, got, allow)
		}
	}

	// A cross-site request with the admin's cookie changes nothing: 403 csrf_failed. Without a JSON Content-Type:
	// 415, also for a DELETE with no body.
	unsafe := []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/auth/register", `{"username":"Sam","password":"` + testPassword + `"}`},
		{http.MethodPost, "/api/v1/auth/invite/check", `{"token":"x"}`},
		{http.MethodPost, "/api/v1/invites", `{}`},
		{http.MethodDelete, "/api/v1/invites/" + invite.ID, ``},
		{http.MethodPost, "/api/v1/admin/approvals/all/reject", `{"all":true}`},
		{http.MethodPost, "/api/v1/admin/approvals/x/approve", `{}`},
		{http.MethodPatch, "/api/v1/admin/settings", `{"registrationMode":"closed"}`},
	}
	for _, u := range unsafe {
		rec := f.call(u.method, u.path, u.body, with(cookie), header("Sec-Fetch-Site", "cross-site"), header("Origin", "https://evil.example"))
		wantError(t, rec, http.StatusForbidden, api.CodeCSRFFailed)
		rec = f.call(u.method, u.path, u.body, with(cookie), header("Content-Type", "text/plain"))
		wantError(t, rec, http.StatusUnsupportedMediaType, api.CodeUnsupportedMediaType)
		rec = f.call(u.method, u.path, u.body, with(cookie), header("Content-Type", ""))
		wantError(t, rec, http.StatusUnsupportedMediaType, api.CodeUnsupportedMediaType)
		// No bearer tokens in M1.
		rec = f.call(u.method, u.path, u.body, with(cookie), header("Authorization", "Bearer isa_"+strings.Repeat("A", 43)))
		wantError(t, rec, http.StatusUnauthorized, api.CodeUnauthenticated)
	}
	if got := f.invites(cookie, ""); len(got) != 1 || f.db.Settings().Get().RegistrationMode != store.ModeInvite || f.tableRows("users") != 1 {
		t.Errorf("refused requests changed something: %d invites, mode %q, %d users", len(got),
			f.db.Settings().Get().RegistrationMode, f.tableRows("users"))
	}
	// The auth endpoints accept 16 KiB, the others 64 KiB.
	big := strings.Repeat("x", authBodyLimit)
	wantError(t, f.call(http.MethodPost, "/api/v1/auth/register", `{"username":"`+big+`"}`), http.StatusRequestEntityTooLarge, api.CodePayloadTooLarge)
	wantError(t, f.call(http.MethodPost, "/api/v1/auth/invite/check", `{"token":"`+big+`"}`), http.StatusRequestEntityTooLarge, api.CodePayloadTooLarge)
	wantError(t, f.call(http.MethodPost, "/api/v1/invites", `{"note":"`+big+`"}`, with(cookie)), http.StatusUnprocessableEntity, api.CodeValidationFailed)
}
