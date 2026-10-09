package auth

import (
	"errors"
	"time"

	"github.com/MoonWX/isshoni/internal/server/store"
)

// Revocation (03 §7.7). Every event that takes credentials away from a user is a row of that section's table: what
// its Write deletes (web sessions, devices with their tokens, push subscriptions, a pending reset link), and which
// live connections are closed afterwards, with which reason. The order is always the same: the Write commits, then
// the session cache forgets what was revoked, and only then does ConnCloser.CloseConnections run. No code path
// deletes a session row without those steps, except the janitor's prune of expired sessions and the startup purge
// after a session-key change (no connection exists yet).
//
// The rows come in two kinds:
//   - one session: logout, revoking one session, the session of the cookie a login, registration or setup arrived
//     with, and the sessions the cap of 50 evicts. Their Write deletes that row, and sessionsRevoked (session.go)
//     runs the steps after the commit;
//   - a user's credentials as a whole: the revocation values below. A Write calls apply, and revocationCommitted
//     runs the steps after the commit.
//
// The M2 rows (revoking one device, a replayed refresh token) come with the device flow; RevokeDevice
// (selfservice.go) already takes one device away. A role change or a rename closes nothing: the hub is told through
// httpapi's Signal.UserChanged, and auth only invalidates the session cache.

// revocation is one row of the table of 03 §7.7 for an event that revokes a user's credentials as a whole.
//
// Push subscriptions are not named here: each one belongs to a web session or a device and goes with it (a foreign
// key with ON DELETE CASCADE), which is the table's "Push subscriptions" column for every row.
type revocation struct {
	// reason is the Reason* code the user's connections are closed with.
	reason string
	// keepCurrent leaves the caller's own web session, and its connections, alone: "all but the current one".
	keepCurrent bool
	// devices takes the user's devices too, with their tokens, and the device codes the user decided (a code that
	// was approved would otherwise still turn into a device).
	devices bool
	// resetLink deletes the user's pending password reset link.
	resetLink bool
}

// The rows of 03 §7.7 that revoke a user's credentials as a whole. The first three and revokeAccountDeleted are
// the user's own actions (selfservice.go). The admin actions come with the admin users slice (03 §18 slice 10):
// its calls run the same two steps with these rows, which is why the whole table is here.
var (
	// "Sign out other browsers": every web session but the caller's. Devices and the reset link stay.
	revokeOtherBrowsers = revocation{reason: ReasonSessionRevoked, keepCurrent: true}
	// "Log out everywhere": every session and device.
	revokeEverywhere = revocation{reason: ReasonLoggedOut, devices: true}
	// "Change own password": every session but the caller's, which the Write also gives a new token, every device,
	// and a pending reset link, which would let its holder set another password.
	revokePasswordChanged = revocation{reason: ReasonPasswordChanged, keepCurrent: true, devices: true, resetLink: true}
	// "Admin: password reset": every session and device. The Write replaces the reset link with the new one
	// instead of deleting it (03 §7.10).
	revokePasswordReset = revocation{reason: ReasonPasswordReset, devices: true}
	// "Admin: sign out everywhere": every session and device.
	revokeSignedOut = revocation{reason: ReasonSessionRevoked, devices: true}
	// "Admin: disable user": every session and device, and a pending reset link.
	revokeAccountDisabled = revocation{reason: ReasonAccountDisabled, devices: true, resetLink: true}
	// "Delete user (admin or self)": everything, by the cascade of the users row. The Write deletes that row and
	// has nothing left to apply; see deleted.
	revokeAccountDeleted = revocation{reason: ReasonAccountDeleted, devices: true, resetLink: true}
)

// revoked is what a revocation took away in its Write: the input of the steps after the commit, and the numbers of
// the audit rows that count ({sessions, devices}).
type revoked struct {
	rule revocation
	user store.UserID
	// kept is the caller's session when the rule keeps it, "" otherwise.
	kept store.SessionID
	// sessions and devices are the deleted rows, sorted. Both are empty after deleted: the cascade does not say
	// what it took.
	sessions []store.SessionID
	devices  []store.DeviceID
}

// errNoCurrentSession is the programming error of applying a rule that keeps the caller's session without saying
// which one that is. It would otherwise delete every session of the user.
var errNoCurrentSession = errors.New("auth: a revocation that keeps the current session needs its ID")

// apply deletes, inside a Write, what the row takes from user: the web sessions (all, or all but current), then,
// as the row says, the devices with the device codes the user decided, and the reset link. current is the caller's
// session and matters only for a row that keeps it. The result goes to revocationCommitted once the Write has
// committed.
func (rv revocation) apply(q *store.Q, user store.UserID, current store.SessionID) (revoked, error) {
	out := revoked{rule: rv, user: user}
	if rv.keepCurrent {
		if current == "" {
			return revoked{}, errNoCurrentSession
		}
		out.kept = current
	}
	var err error
	if out.sessions, err = q.DeleteSessions(user, out.kept); err != nil {
		return revoked{}, err
	}
	if rv.devices {
		if out.devices, err = q.DeleteDevices(user); err != nil {
			return revoked{}, err
		}
		if err := q.DeleteDeviceCodesOf(user); err != nil {
			return revoked{}, err
		}
	}
	if rv.resetLink {
		if err := q.DeletePasswordReset(user); err != nil {
			return revoked{}, err
		}
	}
	return out, nil
}

// deleted is the outcome of a Write that deleted the users row itself: every session, device, push subscription
// and reset link of the user went with it (the cascade of 03 §5), so there was nothing to apply.
func (rv revocation) deleted(user store.UserID) revoked {
	return revoked{rule: rv, user: user}
}

// revocationCommitted runs the steps of 03 §7.7 that follow the commit of a revocation's Write. First the session
// cache forgets the user, so that no request is let in on a cached row (the session the rule kept is read again at
// its next use). Then the user's connections are closed with the row's reason:
//   - a row that takes everything closes all of them: ConnSelector{UserID};
//   - a row that takes everything but the caller's session keeps that session's connections:
//     ConnSelector{UserID, ExceptSessionID};
//   - a row that takes web sessions only ("sign out other browsers") closes exactly the sessions it deleted, one
//     selector each, like the session cap. A selector that excepts one session would also match the connections of
//     the user's devices (M2), which that row keeps.
func (s *Service) revocationCommitted(r revoked) {
	s.sessions.invalidateUser(r.user)
	if s.conns == nil {
		return
	}
	if !r.rule.devices {
		for _, id := range r.sessions {
			s.conns.CloseConnections(ConnSelector{UserID: r.user, SessionID: id}, r.rule.reason)
		}
		return
	}
	s.conns.CloseConnections(ConnSelector{UserID: r.user, ExceptSessionID: r.kept}, r.rule.reason)
}

// ---- the caller of a self-service call ----

// self is the caller of a self-service call as the database has it.
type self struct {
	user    store.User
	session store.Session
	// sessions are all session rows of the account, the caller's included, the most recently seen first (expired
	// ones the janitor has not pruned yet included).
	sessions []store.Session
}

// loadSelf is the chain's authentication again, at the moment of a change: the principal's account, which must be
// active, and its own web session, which must exist and be unexpired at now. Anything else is
// *api.Error{unauthenticated}. The chain let the request in on a principal that may come from the session cache or
// from a read a moment ago, and a logout, a revocation, the account's deletion or an admin may have taken the
// session since: a caller without a session changes nothing. Every self-service Write starts with it.
func loadSelf(q *store.Q, p Principal, now time.Time) (self, error) {
	user, err := q.UserByID(p.UserID)
	if errors.Is(err, store.ErrNotFound) {
		return self{}, errUnauthenticated()
	}
	if err != nil {
		return self{}, err
	}
	if user.Status != store.StatusActive {
		return self{}, errUnauthenticated()
	}
	sessions, err := q.ListSessions(p.UserID)
	if err != nil {
		return self{}, err
	}
	for _, sess := range sessions {
		if sess.ID == p.SessionID {
			if !sessionLive(sess, now) {
				break
			}
			return self{user: user, session: sess, sessions: sessions}, nil
		}
	}
	return self{}, errUnauthenticated()
}

// sessionNamed returns the row with that ID from a user's session list.
func sessionNamed(sessions []store.Session, id store.SessionID) (store.Session, bool) {
	for _, sess := range sessions {
		if sess.ID == id {
			return sess, true
		}
	}
	return store.Session{}, false
}
