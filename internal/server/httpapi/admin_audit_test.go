package httpapi

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// GET /api/v1/admin/audit (README S70; 03 §12.3 #42, §12.4.8). TestAuditCoverage (audit_coverage_test.go) produces
// every action; these tests are about the endpoint.

// entryIDs returns the ids of REST audit entries, in their order.
func entryIDs(entries []api.AuditEntry) []int64 {
	out := make([]int64, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.ID)
	}
	return out
}

// auditPage is GET /api/v1/admin/audit with a query, answered 200.
func (f *authFixture) auditPage(c *http.Cookie, query string) api.AuditPage {
	f.t.Helper()
	return decodeBody[api.AuditPage](f.t, f.call(http.MethodGet, "/api/v1/admin/audit"+query, "", with(c)),
		http.StatusOK)
}

// newestFirst returns the store's audit rows newest first, as the endpoint lists them.
func (f *authFixture) newestFirst() []store.AuditEntry {
	f.t.Helper()
	rows := f.audit()
	slices.Reverse(rows)
	return rows
}

// TestAuditLogOverREST: the log is for admins only; an entry has the shape of 03 §12.4.8, with no target and no IP
// for a CLI action; the filters (an action prefix, the actor, the target) combine; an empty filter is none.
func TestAuditLogOverREST(t *testing.T) {
	f := newAuthFixture(t)
	admin, cookie, member, memberCookie := f.adminAndMember()

	wantError(t, f.call(http.MethodGet, "/api/v1/admin/audit", "", with(memberCookie)), http.StatusForbidden,
		api.CodeForbidden)
	wantError(t, f.call(http.MethodGet, "/api/v1/admin/audit", ""), http.StatusUnauthorized, api.CodeUnauthenticated)

	// The setup's two rows, exactly: the CLI's has no target and no IP, the wizard's has both.
	issued, completed := f.auditOf("setup.token_issued"), f.auditOf("setup.completed")
	if len(issued) != 1 || len(completed) != 1 {
		t.Fatalf("setup rows: %d issued, %d completed", len(issued), len(completed))
	}
	rec := f.call(http.MethodGet, "/api/v1/admin/audit?action=setup.", "", with(cookie))
	want := fmt.Sprintf(`{"entries":[`+
		`{"id":%d,"action":"setup.completed","outcome":"ok","actor":{"kind":"user","id":"%s","name":"Alex"},`+
		`"target":{"kind":"user","id":"%[2]s","name":"Alex"},"ip":"203.0.113.7","detail":{},`+
		`"at":"2026-10-01T12:00:00.000Z"},`+
		`{"id":%d,"action":"setup.token_issued","outcome":"ok","actor":{"kind":"cli","name":"cli"},"detail":{},`+
		`"at":"2026-10-01T12:00:00.000Z"}],"nextBefore":null}`+"\n", completed[0].ID, admin.ID, issued[0].ID)
	if rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Fatalf("GET /admin/audit?action=setup. = %d %s\nwant %s", rec.Code, rec.Body, want)
	}

	// More rows: a failed login (anonymous, target Sam) and Sam's own logout.
	wantError(t, f.login("Sam", "not the password", addrB), http.StatusUnauthorized, api.CodeInvalidCredentials)
	wantNoContent(t, f.call(http.MethodPost, "/api/v1/auth/logout", `{}`, with(memberCookie), from(addrC)))
	all := f.newestFirst()
	actions := func(page api.AuditPage) string {
		var out []string
		for _, e := range page.Entries {
			out = append(out, e.Action)
		}
		return strings.Join(out, " ")
	}
	for _, tc := range []struct{ query, want string }{
		{"", "auth.logout auth.login_failed user.registered invite.created setup.completed setup.token_issued"},
		{"?action=&actor=&target=", "auth.logout auth.login_failed user.registered invite.created setup.completed " +
			"setup.token_issued"},
		{"?unknown=1", "auth.logout auth.login_failed user.registered invite.created setup.completed setup.token_issued"},
		{"?action=auth.", "auth.logout auth.login_failed"},
		{"?action=auth.login", "auth.login_failed"}, // a prefix
		{"?action=user.", "user.registered"},
		{"?action=nothing.", ""},
		{"?actor=" + admin.ID, "invite.created setup.completed"},
		{"?actor=" + member.ID, "auth.logout user.registered"},
		{"?target=" + member.ID, "auth.login_failed user.registered"},
		{"?target=" + member.ID + "&actor=" + member.ID, "user.registered"},
		{"?target=" + member.ID + "&action=auth.", "auth.login_failed"},
		{"?actor=" + admin.ID + "&action=user.", ""},
		{"?before=" + strconv.FormatInt(all[1].ID, 10), "user.registered invite.created setup.completed " +
			"setup.token_issued"},
		{"?before=" + strconv.FormatInt(all[1].ID, 10) + "&action=setup.", "setup.completed setup.token_issued"},
		{"?before=1", ""},
	} {
		page := f.auditPage(cookie, tc.query)
		if got := actions(page); got != tc.want || page.NextBefore != nil {
			t.Errorf("GET /admin/audit%s = [%s] nextBefore %v; want [%s] and null", tc.query, got, page.NextBefore,
				tc.want)
		}
	}

	// The failed login's row: anonymous, from B, with Sam as its target; Sam's logout from C.
	page := f.auditPage(cookie, "?limit=2")
	failed, logout := page.Entries[1], page.Entries[0]
	if failed.Actor != (api.AuditRef{Kind: "anonymous"}) || failed.IP != addrB || failed.Outcome != api.AuditOutcomeDenied ||
		failed.Target == nil || *failed.Target != (api.AuditRef{Kind: "user", ID: member.ID, Name: "Sam"}) ||
		fmt.Sprint(failed.Detail) != "map[reason:wrong_password]" {
		t.Errorf("the failed login's entry = %+v", failed)
	}
	if logout.Actor != (api.AuditRef{Kind: "user", ID: member.ID, Name: "Sam"}) || logout.IP != addrC ||
		logout.Target == nil || logout.Target.Kind != "session" || !logout.At.Equal(f.clk.now()) {
		t.Errorf("the logout's entry = %+v", logout)
	}
}

// TestAuditLogPaging: pages follow the id cursor. nextBefore is the id of a page's last entry while older rows
// match, and null on the last page, a full one too. Rows that arrive while a client pages do not move its pages.
func TestAuditLogPaging(t *testing.T) {
	f := newAuthFixture(t)
	_, cookie := f.setupAdmin("Alex")
	for i := range 8 {
		wantError(t, f.login("Alex", "not the password", clientIP(i)), http.StatusUnauthorized,
			api.CodeInvalidCredentials)
	}
	all := f.newestFirst() // 2 setup rows and 8 failed logins
	if len(all) != 10 {
		t.Fatalf("%d audit rows, want 10", len(all))
	}
	var ids []int64
	for _, e := range all {
		ids = append(ids, e.ID)
	}

	// limit 3: pages of 3, 3, 3, 1.
	var got []int64
	query := "?limit=3"
	for range 4 {
		page := f.auditPage(cookie, query)
		got = append(got, entryIDs(page.Entries)...)
		if page.NextBefore == nil {
			break
		}
		if *page.NextBefore != got[len(got)-1] {
			t.Fatalf("nextBefore %d, want the page's last id %d", *page.NextBefore, got[len(got)-1])
		}
		query = "?limit=3&before=" + strconv.FormatInt(*page.NextBefore, 10)
	}
	if !slices.Equal(got, ids) {
		t.Fatalf("paged ids %v, want %v", got, ids)
	}

	// A full last page has no next one; one row more has.
	if page := f.auditPage(cookie, "?limit=10"); len(page.Entries) != 10 || page.NextBefore != nil {
		t.Errorf("limit=10 over 10 rows: %d entries, nextBefore %v; want 10 and null", len(page.Entries), page.NextBefore)
	}
	if page := f.auditPage(cookie, "?limit=9"); len(page.Entries) != 9 || page.NextBefore == nil || *page.NextBefore != ids[8] {
		t.Errorf("limit=9 over 10 rows: %d entries, nextBefore %v; want 9 and %d", len(page.Entries), page.NextBefore, ids[8])
	}
	// The default is 50 rows.
	if page := f.auditPage(cookie, ""); len(page.Entries) != 10 || page.NextBefore != nil {
		t.Errorf("no limit over 10 rows: %d entries, nextBefore %v", len(page.Entries), page.NextBefore)
	}

	// A client reads the first page, new rows arrive, and the next pages are the old rows below it.
	first := f.auditPage(cookie, "?limit=4")
	f.clk.advance(time.Minute)
	for i := range 3 {
		wantError(t, f.login("Alex", "still not the password", clientIP(20+i)), http.StatusUnauthorized,
			api.CodeInvalidCredentials)
	}
	got = entryIDs(first.Entries)
	for next := first.NextBefore; next != nil; {
		page := f.auditPage(cookie, "?limit=4&before="+strconv.FormatInt(*next, 10))
		got = append(got, entryIDs(page.Entries)...)
		next = page.NextBefore
	}
	if !slices.Equal(got, ids) {
		t.Fatalf("paging while rows arrived: ids %v, want %v", got, ids)
	}
	if page := f.auditPage(cookie, "?limit=4"); page.Entries[0].ID <= ids[0] || len(page.Entries) != 4 {
		t.Fatalf("the newest page after the new rows starts at %d", page.Entries[0].ID)
	}
}

// TestAuditLogBadQuery: 400 bad_request for a query the endpoint can't read (03 §12.3 #42), the limits included.
func TestAuditLogBadQuery(t *testing.T) {
	f := newAuthFixture(t)
	_, cookie := f.setupAdmin("Alex")
	long := strings.Repeat("a", 65)
	for _, q := range []string{
		"before=0", "before=-1", "before=abc", "before=1.5", "before=", "before=1&before=2",
		"before=99999999999999999999",
		"limit=0", "limit=201", "limit=-5", "limit=x", "limit=", "limit=1&limit=2", "limit=1e2",
		"action=" + long, "actor=" + long, "target=" + long, "action=a&action=b", "actor=&actor=", "target=a&target=",
	} {
		wantError(t, f.call(http.MethodGet, "/api/v1/admin/audit?"+q, "", with(cookie)), http.StatusBadRequest,
			api.CodeBadRequest)
	}
	for _, q := range []string{"limit=1", "limit=200", "before=9223372036854775807", "action=" + long[:64],
		"actor=" + long[:64], "target=" + long[:64], "before=%2B5"} {
		if rec := f.call(http.MethodGet, "/api/v1/admin/audit?"+q, "", with(cookie)); rec.Code != http.StatusOK {
			t.Errorf("?%s: status %d, want 200", q, rec.Code)
		}
	}
}

// TestAuditRowsPrunedAfter30Days: the janitor removes audit rows older than 30 days (03 §10, §4.7); a row exactly
// 30 days old stays until the next run.
func TestAuditRowsPrunedAfter30Days(t *testing.T) {
	f := newAuthFixture(t)
	_, cookie := f.setupAdmin("Alex")
	now := f.clk.now()
	f.write(func(q *store.Q) error {
		for _, e := range []store.AuditEntry{
			{At: now.Add(-30*24*time.Hour - time.Millisecond), Action: "room.created", Actor: store.CLIActor},
			{At: now.Add(-30 * 24 * time.Hour), Action: "room.renamed", Actor: store.CLIActor},
		} {
			if err := q.AppendAudit(e); err != nil {
				return err
			}
		}
		return nil
	})
	if got := f.auditPage(cookie, "?action=room."); len(got.Entries) != 2 {
		t.Fatalf("%d room rows before the janitor, want 2", len(got.Entries))
	}
	f.runJanitor(f.svc, func() bool { return len(f.auditOf("room.created")) == 0 })
	got := f.auditPage(cookie, "?action=room.")
	if len(got.Entries) != 1 || got.Entries[0].Action != "room.renamed" {
		t.Fatalf("room rows after the janitor: %+v; want only the one exactly 30 days old", got.Entries)
	}
	if n := len(f.auditPage(cookie, "?action=setup.").Entries); n != 2 {
		t.Fatalf("%d setup rows after the janitor, want 2", n)
	}
}

// FuzzParseAuditQuery: whatever the query, the parser answers bad_request or a query inside the limits of 03 §6:
// a limit of 1–200, a cursor that is 0 or positive, and text filters of at most 64 bytes, each the parameter's one
// value.
func FuzzParseAuditQuery(f *testing.F) {
	for _, s := range []string{"", "limit=50", "before=4812&limit=50&action=user.&actor=k3m9p2qxw7ht&target=b8f2n4r6t0vz",
		"before=0", "before=-1", "before=%2B7", "before=9223372036854775808", "limit=200", "limit=201", "limit=007",
		"limit=1&limit=1", "action=" + strings.Repeat("a", 65), "action=%ff%fe", "action=&actor=&target=", "x=1&limit=3",
		"before=1e3", "limit= 5", ";limit=5", "limit=%00"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		v, _ := url.ParseQuery(raw) // what r.URL.Query() keeps of a malformed query
		q, err := parseAuditQuery(v)
		if err != nil {
			if !api.IsCode(err, api.CodeBadRequest) {
				t.Fatalf("parseAuditQuery(%q): %v, want bad_request", raw, err)
			}
			return
		}
		if q.Limit < 1 || q.Limit > auditMaxLimit || q.Before < 0 || len(q.ActionPrefix) > auditMaxFilter ||
			len(q.ActorID) > auditMaxFilter || len(q.TargetID) > auditMaxFilter {
			t.Fatalf("parseAuditQuery(%q) = %+v: outside the limits", raw, q)
		}
		for name, got := range map[string]string{auditQueryAction: q.ActionPrefix, auditQueryActor: string(q.ActorID),
			auditQueryTarget: q.TargetID} {
			if vals := v[name]; got != "" && (len(vals) != 1 || vals[0] != got) {
				t.Fatalf("parseAuditQuery(%q): %s = %q from %q", raw, name, got, vals)
			}
		}
	})
}
