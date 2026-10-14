package httpapi

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// The audit log for admins (README S70; 03 §10, §12.3 #42, §12.4.8). The rows are written by the changes they
// describe, in the same transaction (auth.Service, the room handlers, the settings cache, the janitor); this file
// only reads them.

// Query parameters of GET /api/v1/admin/audit (03 §12.4.8).
const (
	auditQueryBefore = "before" // the id cursor: rows with a smaller id; absent = from the newest
	auditQueryLimit  = "limit"  // 1–200 rows; absent = 50
	auditQueryAction = "action" // a prefix of the action, e.g. "user." or "auth.login_failed"
	auditQueryActor  = "actor"  // the acting user's ID
	auditQueryTarget = "target" // the target's ID: a user, session, device, invite or room
)

// Limits of an audit query (03 §6 AuditQuery, §12.4.8).
const (
	auditDefaultLimit = 50
	auditMaxLimit     = 200
	// auditMaxFilter caps the text filters. Actions are short dotted names and IDs are 12 characters (the default
	// room's is "lounge"); a longer value can match nothing.
	auditMaxFilter = 64
)

// apiAuditEntry is an audit row as REST shows it (03 §12.4.8): the actor always, the target when the action has
// one, the IP when the row has one (it is "" for the CLI and the server's own jobs), and the detail, {} when empty.
// The names are the snapshots taken when the row was written.
func apiAuditEntry(e store.AuditEntry) api.AuditEntry {
	out := api.AuditEntry{
		ID:      e.ID,
		At:      e.At,
		Action:  e.Action,
		Outcome: api.AuditOutcome(e.Outcome),
		Actor:   api.AuditRef{Kind: string(e.Actor.Kind), ID: string(e.Actor.UserID), Name: e.Actor.Name},
		IP:      e.Actor.IP,
		Detail:  e.Detail,
	}
	if out.Detail == nil {
		out.Detail = map[string]any{}
	}
	if e.TargetKind != "" {
		out.Target = &api.AuditRef{Kind: e.TargetKind, ID: e.TargetID, Name: e.TargetName}
	}
	return out
}

// apiAuditEntries converts a list of audit rows, keeping their order; an empty list is [] in JSON.
func apiAuditEntries(rows []store.AuditEntry) []api.AuditEntry {
	out := make([]api.AuditEntry, 0, len(rows))
	for _, e := range rows {
		out = append(out, apiAuditEntry(e))
	}
	return out
}

// parseAuditQuery reads the query of GET /api/v1/admin/audit. Every parameter is optional and may appear once; an
// empty text filter is no filter. 400 bad_request for a parameter given twice, a before that is not a positive
// integer, a limit that is not an integer from 1 to 200, and a text filter over 64 bytes. Other parameters are
// ignored.
func parseAuditQuery(v url.Values) (store.AuditQuery, error) {
	bad := api.NewError(api.CodeBadRequest)
	q := store.AuditQuery{Limit: auditDefaultLimit}
	for _, name := range []string{auditQueryBefore, auditQueryLimit, auditQueryAction, auditQueryActor, auditQueryTarget} {
		vals, ok := v[name]
		if !ok {
			continue
		}
		if len(vals) != 1 {
			return store.AuditQuery{}, bad
		}
		s := vals[0]
		switch name {
		case auditQueryBefore:
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil || n < 1 {
				return store.AuditQuery{}, bad
			}
			q.Before = n
		case auditQueryLimit:
			n, err := strconv.Atoi(s)
			if err != nil || n < 1 || n > auditMaxLimit {
				return store.AuditQuery{}, bad
			}
			q.Limit = n
		default:
			if len(s) > auditMaxFilter {
				return store.AuditQuery{}, bad
			}
			switch name {
			case auditQueryAction:
				q.ActionPrefix = s
			case auditQueryActor:
				q.ActorID = store.UserID(s)
			case auditQueryTarget:
				q.TargetID = s
			}
		}
	}
	return q, nil
}

// getAudit is GET /api/v1/admin/audit (03 §12.3 #42, §12.4.8): ?before&limit&action&actor&target →
// {entries, nextBefore}, the rows newest first. The filters combine: an action prefix ("user." is every account
// action), the acting user's ID and the target's ID. A page is the rows below the before cursor, so a client that
// pages with nextBefore sees every row once even while new rows arrive (they get higher ids). nextBefore is the id
// of the page's last row when older rows match, and null on the last page; a full page with nothing after it is the
// last page too. Errors: 400 bad_request for a bad query (parseAuditQuery). It reads only, in one snapshot.
func (a *API) getAudit(w http.ResponseWriter, r *http.Request) {
	query, err := parseAuditQuery(r.URL.Query())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var rows []store.AuditEntry
	more := false
	err = a.d.DB.Read(r.Context(), func(q *store.Q) error {
		var err error
		if rows, err = q.ListAudit(query); err != nil || len(rows) < query.Limit {
			return err
		}
		// A full page: one more row below it means another page.
		next := query
		next.Before, next.Limit = rows[len(rows)-1].ID, 1
		rest, err := q.ListAudit(next)
		more = len(rest) > 0
		return err
	})
	if err != nil {
		WriteError(w, r, opErr("audit", err))
		return
	}
	page := api.AuditPage{Entries: apiAuditEntries(rows)}
	if more {
		last := rows[len(rows)-1].ID
		page.NextBefore = &last
	}
	WriteJSON(w, http.StatusOK, page)
}
