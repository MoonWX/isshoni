package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// settingsPatch builds a settings patch from JSON text per field.
func settingsPatch(kv ...string) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = json.RawMessage(kv[i+1])
	}
	return out
}

func TestUpdateSettings(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	alex := e.addAdmin("Alex")
	admin := actorFor(alex, ipA)
	defaults := e.db.Settings().Defaults()
	var changes []string
	cancel := e.db.Settings().OnChange(func(prev, next store.Settings) {
		changes = append(changes, fmt.Sprintf("%s→%s", prev.RegistrationMode, next.RegistrationMode))
	})
	defer cancel()

	// A change that leaves the registration mode alone: written, audited, no alert.
	ch, err := e.svc.UpdateSettings(ctx, admin, settingsPatch("serverName", `"  Alex's  server "`, "maxSharesPerRoom", `4`))
	if err != nil {
		t.Fatal(err)
	}
	want := defaults
	want.ServerName, want.MaxSharesPerRoom = "Alex's server", 4
	if ch.Before != defaults || ch.After != want || e.db.Settings().Get() != want {
		t.Errorf("UpdateSettings = %+v; want %+v → %+v", ch, defaults, want)
	}
	if alerts := e.alerts.take(); len(alerts) != 0 {
		t.Errorf("a change without the registration mode raised %+v", alerts)
	}
	rows := e.audit("settings.changed")
	if len(rows) != 1 || rows[0].Actor != admin || rows[0].TargetKind != "settings" {
		t.Fatalf("settings.changed rows = %+v", rows)
	}

	// The registration mode: the alert, with the admin's name and no target, after the commit.
	e.advance(time.Minute)
	now := e.clk.now()
	ch, err = e.svc.UpdateSettings(ctx, admin, settingsPatch("registrationMode", `"approval"`))
	if err != nil {
		t.Fatal(err)
	}
	if ch.Before.RegistrationMode != store.ModeInvite || ch.After.RegistrationMode != store.ModeApproval {
		t.Errorf("change = %+v", ch)
	}
	alerts := e.alerts.take()
	if len(alerts) != 1 || alerts[0] != (AdminAlert{Kind: AlertRegistrationModeChanged, Actor: "Alex", At: now}) {
		t.Fatalf("alerts = %+v, want one registration_mode_changed by Alex", alerts)
	}
	if !strings.Contains(e.logs.String(), "kind=registration_mode_changed") {
		t.Error("the alert's WARN line is missing")
	}
	// The row is a security event (03 §10).
	e.read(func(q *store.Q) error {
		events, err := q.SecurityEvents(now.Add(-time.Hour), 0)
		if err == nil && (len(events) != 1 || events[0].Action != "settings.changed" ||
			!strings.Contains(fmt.Sprint(events[0].Detail), "registrationMode")) {
			t.Errorf("security events = %+v, want the registration-mode change", events)
		}
		return err
	})

	// A patch that changes nothing: no row, no alert, and Before equals After.
	n := e.auditCount("settings.changed")
	for _, p := range []map[string]json.RawMessage{
		settingsPatch(),
		settingsPatch("registrationMode", `"approval"`),
		settingsPatch("nope", `1`),
		nil,
	} {
		ch, err := e.svc.UpdateSettings(ctx, admin, p)
		if err != nil || ch.Before != ch.After || ch.After != e.db.Settings().Get() {
			t.Errorf("a no-op patch %v = %+v, %v", p, ch, err)
		}
	}
	if e.auditCount("settings.changed") != n || len(e.alerts.take()) != 0 {
		t.Error("a no-op patch wrote an audit row or raised an alert")
	}

	// Errors are the store's: a bad value and null are validation_failed, a pinned field is setting_locked.
	_, err = e.svc.UpdateSettings(ctx, admin, settingsPatch("registrationMode", `"open"`, "inviteDefaultMaxUses", `null`))
	if ae := wantCode(t, err, api.CodeValidationFailed); fmt.Sprint(ae.Fields) != "map[inviteDefaultMaxUses:invalid registrationMode:invalid]" {
		t.Errorf("fields = %v", ae.Fields)
	}
	if err := e.db.Settings().Pin("registrationMode", "closed"); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{`"closed"`, `"invite"`, `"open"`} {
		_, err = e.svc.UpdateSettings(ctx, admin, settingsPatch("registrationMode", v))
		if ae := wantCode(t, err, api.CodeSettingLocked); fmt.Sprint(ae.Params) != "map[field:registrationMode]" {
			t.Errorf("setting_locked params = %v", ae.Params)
		}
	}
	if e.auditCount("settings.changed") != n || len(e.alerts.take()) != 0 {
		t.Error("a refused patch wrote an audit row or raised an alert")
	}
	// The pin changed the mode in force (an OnChange call), but nobody patched it: no alert for it.
	if got := fmt.Sprint(changes); got != "[invite→invite invite→approval approval→closed]" {
		t.Errorf("OnChange calls = %s", got)
	}

	// The CLI changes settings too; its alert names it.
	if _, err := e.svc.UpdateSettings(ctx, store.CLIActor, settingsPatch("membersCanInvite", `true`)); err != nil {
		t.Fatal(err)
	}
	if rows := e.audit("settings.changed"); rows[len(rows)-1].Actor != store.CLIActor {
		t.Errorf("the CLI's settings.changed row has the actor %+v", rows[len(rows)-1].Actor)
	}

	// Only an active admin changes settings.
	sam := e.addUser("Sam")
	for name, a := range map[string]store.Actor{
		"a member":   actorFor(sam, ipB),
		"anonymous":  {Kind: store.ActorAnonymous},
		"the system": store.SystemActor,
	} {
		if _, err := e.svc.UpdateSettings(ctx, a, settingsPatch("maxSharesPerRoom", `9`)); !api.IsCode(err, api.CodeForbidden) {
			t.Errorf("UpdateSettings as %s: %v, want forbidden", name, err)
		}
	}
	e.write(func(q *store.Q) error { return q.SetStatus(alex.ID, store.StatusDisabled, e.clk.now()) })
	_, err = e.svc.UpdateSettings(ctx, admin, settingsPatch("maxSharesPerRoom", `9`))
	wantCode(t, err, api.CodeForbidden)
	if got := e.db.Settings().Get().MaxSharesPerRoom; got != 4 {
		t.Errorf("maxSharesPerRoom = %d after refused patches, want 4", got)
	}
}

// TestUpdateSettingsParallel: two admins who save at once each see exactly their own change, so every change of
// the registration mode raises its alert and a save that changed nothing raises none.
func TestUpdateSettingsParallel(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	alex, kim := e.addAdmin("Alex"), e.addAdmin("Kim")
	modes := []string{`"invite"`, `"approval"`, `"closed"`}
	const rounds = 30
	var wg sync.WaitGroup
	var mu sync.Mutex
	changed := 0
	for i, u := range []store.User{alex, kim} {
		wg.Go(func() {
			for r := range rounds {
				ch, err := e.svc.UpdateSettings(ctx, actorFor(u, ipA), settingsPatch("registrationMode", modes[(r+i)%len(modes)]))
				if err != nil {
					t.Errorf("UpdateSettings: %v", err)
					return
				}
				if ch.Before.RegistrationMode != ch.After.RegistrationMode {
					mu.Lock()
					changed++
					mu.Unlock()
				}
			}
		})
	}
	wg.Wait()
	alerts := e.alerts.take()
	rows := e.audit("settings.changed")
	if len(alerts) != changed || len(rows) != changed || changed == 0 {
		t.Fatalf("%d changes seen by their callers, %d alerts, %d settings.changed rows; want the same number",
			changed, len(alerts), len(rows))
	}
}

// TestUpdateSettingsActorAtTheChange: the acting admin is read in the Write that changes the settings (actingAdmin),
// so an admin who is disabled or demoted while their patch waits for the writer changes nothing and raises no alert.
func TestUpdateSettingsActorAtTheChange(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	before := e.db.Settings().Get()
	cases := []struct {
		name   string
		revoke func(q *store.Q, id store.UserID) error
	}{
		{"Alex", func(q *store.Q, id store.UserID) error { return q.SetStatus(id, store.StatusDisabled, e.clk.now()) }},
		{"Kim", func(q *store.Q, id store.UserID) error { return q.SetRole(id, store.RoleUser, e.clk.now()) }},
	}
	for _, tc := range cases {
		admin := e.addAdmin(tc.name)
		errc := make(chan error, 1)
		// The test holds the one writer while it takes the admin's rights away. Until that commits, every reader still
		// sees an active admin, and the patch can only queue for the writer.
		e.write(func(q *store.Q) error {
			if err := tc.revoke(q, admin.ID); err != nil {
				return err
			}
			go func() {
				_, err := e.svc.UpdateSettings(ctx, actorFor(admin, ipA), settingsPatch("registrationMode", `"approval"`))
				errc <- err
			}()
			// Time for the call to get as far as it can before the commit. The outcome does not depend on it: a call
			// that starts after the commit finds the admin's rights gone as well.
			time.Sleep(50 * time.Millisecond)
			return nil
		})
		if err := <-errc; !api.IsCode(err, api.CodeForbidden) {
			t.Errorf("UpdateSettings as %s, who lost the admin's rights before the write: %v, want forbidden", tc.name, err)
		}
	}
	if got := e.db.Settings().Get(); got != before {
		t.Errorf("settings = %+v after refused patches, want them unchanged", got)
	}

	// The actor comes first: someone who is not an admin doesn't learn whether a value is valid or a field is pinned.
	if err := e.db.Settings().Pin("maxSharesPerRoom", 2); err != nil {
		t.Fatal(err)
	}
	sam := e.addUser("Sam")
	for _, p := range []map[string]json.RawMessage{
		settingsPatch("registrationMode", `"open"`),
		settingsPatch("maxSharesPerRoom", `2`),
		settingsPatch(),
	} {
		if _, err := e.svc.UpdateSettings(ctx, actorFor(sam, ipB), p); !api.IsCode(err, api.CodeForbidden) {
			t.Errorf("UpdateSettings as a member with %v: %v, want forbidden", p, err)
		}
	}
	if e.auditCount("settings.changed") != 0 || len(e.alerts.take()) != 0 {
		t.Error("a refused patch wrote an audit row or raised an alert")
	}
}
