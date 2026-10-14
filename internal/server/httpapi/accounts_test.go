package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// API.DashboardAccounts, the accounts part of 04's admin dashboard (README S70; 03 §12.4.8).

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// accountsGolden is the golden file of TestDashboardAccountsGolden.
const accountsGolden = "testdata/dashboard_accounts.json"

// encodeGolden is the encoding of a golden file, as internal/protocol/api's: two-space indent, no HTML escaping and
// a final newline.
func encodeGolden(t *testing.T, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	return buf.Bytes()
}

// TestDashboardAccountsGolden builds the accounts object over a server with one of everything and compares it with
// testdata/dashboard_accounts.json (the user IDs, which the store draws at random, stand there as <name>):
//   - users: Alex, Sam and Pat active (Alex and Pat admins), Dee an admin who is disabled (no admin any more),
//     Kim and Lee waiting for approval;
//   - invites: one in use and one unused count; a revoked, a used-up and an expired one do not;
//   - sessions: Alex's two, Sam's and Pat's; Dee's went with the account, and a row past its idle expiry that the
//     janitor has not pruned yet does not count;
//   - devices: the one row M2's linking would have written;
//   - the registration mode in force;
//   - security events: the rows of §10's Sec column from the last 7 days, newest first. The setup, 8 days ago, is
//     outside; the settings change that was not the registration mode, the logins and the invites are no
//     security events.
func TestDashboardAccountsGolden(t *testing.T) {
	f := newAuthFixture(t)
	f.setupAdmin("Alex")
	f.clk.advance(8 * 24 * time.Hour)
	admin := f.sessionCookie(f.login("Alex", testPassword, addrA), thirtyDays)

	_, inUse := f.createInvite(admin, `{"maxUses":10,"note":"friends"}`)
	f.createInvite(admin, `{}`)
	revoked, _ := f.createInvite(admin, `{}`)
	wantNoContent(t, f.call(http.MethodDelete, "/api/v1/invites/"+revoked.ID, "", with(admin)))
	_, single := f.createInvite(admin, `{"maxUses":1}`)
	f.createInvite(admin, `{"expiresInHours":1}`)

	sam, _ := f.join(inUse, "Sam", clientIP(1))
	pat, _ := f.join(inUse, "Pat", clientIP(2))
	dee, _ := f.join(single, "Dee", clientIP(3))
	f.clk.advance(time.Minute)
	f.patched(admin, pat.ID, `{"role":"admin","currentPassword":"`+testPassword+`"}`)
	f.clk.advance(time.Minute)
	f.patched(admin, dee.ID, `{"role":"admin","currentPassword":"`+testPassword+`"}`)
	f.clk.advance(time.Minute)
	f.patched(admin, dee.ID, `{"status":"disabled"}`)

	// A throttle (a security event), while sign-ups still need an invite; then the approval mode and two sign-ups.
	f.clk.advance(time.Minute)
	for range 5 {
		wantError(t, f.register("", "Nobody", clientIP(7)), http.StatusForbidden, api.CodeInviteRequired)
	}
	wantError(t, f.register("", "Nobody", clientIP(7)), http.StatusTooManyRequests, api.CodeRateLimited)
	f.clk.advance(time.Minute)
	f.setMode(admin, "approval")
	f.clk.advance(time.Minute)
	if rec := f.patchSettings(admin, `{"maxSharesPerRoom":4}`); rec.Code != http.StatusOK {
		t.Fatalf("PATCH settings: %d %s", rec.Code, rec.Body)
	}
	f.signup("Kim", clientIP(4))
	f.signup("Lee", clientIP(5))

	// A session past its idle expiry that the janitor has not pruned, and a linked device (M2 writes those).
	now := f.clk.now()
	f.write(func(q *store.Q) error {
		return q.CreateSession(&store.Session{UserID: store.UserID(sam.ID), TokenHash: []byte("an expired session"),
			Name: "Firefox on Linux", CreatedAt: now.Add(-40 * 24 * time.Hour), LastSeenAt: now.Add(-31 * 24 * time.Hour),
			IdleExpiresAt: now.Add(-24 * time.Hour), ExpiresAt: now.Add(140 * 24 * time.Hour)})
	})
	f.linkDevice(sam.ID, "d1d1d1d1d1d1")
	f.clk.advance(2 * time.Hour) // the one-hour invite expires

	acc, err := f.api.DashboardAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := encodeGolden(t, acc)
	var names []string
	f.read(func(q *store.Q) error {
		users, err := q.ListUsers("")
		for _, u := range users {
			names = append(names, string(u.ID), "<"+u.Username+">")
		}
		return err
	})
	got = []byte(strings.NewReplacer(names...).Replace(string(got)))
	if *update {
		if err := os.WriteFile(filepath.FromSlash(accountsGolden), got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(filepath.FromSlash(accountsGolden))
	if err != nil {
		t.Fatalf("%v (run go test -run TestDashboardAccountsGolden -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("DashboardAccounts:\n%s\nwant (%s):\n%s", got, accountsGolden, want)
	}

	// The same object decodes into the DTO with no field left over.
	dec := json.NewDecoder(bytes.NewReader(want))
	dec.DisallowUnknownFields()
	var back api.DashboardAccounts
	if err := dec.Decode(&back); err != nil {
		t.Fatal(err)
	}
	if back.Users != acc.Users || back.Sessions != acc.Sessions || len(back.SecurityEvents) != len(acc.SecurityEvents) {
		t.Fatalf("decoded %+v, built %+v", back, acc)
	}
}

// TestDashboardAccountsSecurityEvents: at most 20 rows, newest first, from the last 7 days (a row exactly 7 days old
// is in); only the actions of §10's Sec column, and settings.changed only when it changed the registration mode.
func TestDashboardAccountsSecurityEvents(t *testing.T) {
	f := newAuthFixture(t)
	ctx := context.Background()
	empty, err := f.api.DashboardAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(encodeGolden(t, empty)); !strings.Contains(got, `"securityEvents": []`) ||
		!strings.Contains(got, `"registrationMode": "invite"`) {
		t.Fatalf("a new server's accounts:\n%s", got)
	}

	now := f.clk.now()
	week := 7 * 24 * time.Hour
	add := func(at time.Time, action string, detail map[string]any) int64 {
		t.Helper()
		var id int64
		f.write(func(q *store.Q) error {
			if err := q.AppendAudit(store.AuditEntry{At: at, Action: action, Actor: store.CLIActor, Detail: detail}); err != nil {
				return err
			}
			rows, err := q.ListAudit(store.AuditQuery{Limit: 1})
			if len(rows) == 1 {
				id = rows[0].ID
			}
			return err
		})
		return id
	}
	add(now.Add(-week-time.Millisecond), "user.disabled", nil) // too old
	atLimit := add(now.Add(-week), "user.deleted", nil)
	add(now.Add(-time.Hour), "settings.changed", map[string]any{"changes": map[string]any{
		"maxSharesPerRoom": map[string]any{"from": 0, "to": 4}}}) // no security event
	mode := add(now.Add(-time.Hour), "settings.changed", map[string]any{"changes": map[string]any{
		"registrationMode": map[string]any{"from": "invite", "to": "closed"}}})
	add(now, "auth.login", nil)
	add(now, "user.enabled", nil)
	rotated := add(now, "secrets.rotated", map[string]any{"keys": []string{"invite"}})

	ids := func() []int64 {
		t.Helper()
		acc, err := f.api.DashboardAccounts(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return entryIDs(acc.SecurityEvents)
	}
	if got, want := ids(), []int64{rotated, mode, atLimit}; !slices.Equal(got, want) {
		t.Fatalf("security events %v, want %v", got, want)
	}

	// 25 more: the newest 20 of all.
	var newest []int64
	for i := range 25 {
		newest = append(newest, add(now.Add(time.Duration(i)*time.Second), "auth.throttled",
			map[string]any{"scope": "ip", "key": "192.0.2." + strconv.Itoa(i)}))
	}
	want := make([]int64, 0, 20)
	for i := len(newest) - 1; len(want) < 20; i-- {
		want = append(want, newest[i])
	}
	if got := ids(); !slices.Equal(got, want) {
		t.Fatalf("security events %v, want the newest 20, %v", got, want)
	}
}

// TestDashboardAccountsStoreError: a store that fails gives an error, which 04's dashboard turns into its
// dashboard.source_failed alert (04 §11.4).
func TestDashboardAccountsStoreError(t *testing.T) {
	f := newAuthFixture(t)
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.api.DashboardAccounts(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "httpapi: dashboard accounts") {
		t.Fatalf("DashboardAccounts on a closed store: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f2 := newAuthFixture(t)
	if _, err := f2.api.DashboardAccounts(ctx); err == nil {
		t.Fatal("DashboardAccounts with a cancelled context: no error")
	}
}
