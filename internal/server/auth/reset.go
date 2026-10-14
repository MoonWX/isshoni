package auth

import (
	"context"
	"errors"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// Admin-issued password resets (03 §7.10; README S64). There is no e-mail: an admin, or the operator's CLI for an
// admin who is locked out, issues a one-time link and passes it on privately. Issuing takes the old password and
// every credential of the account away at once; the link's holder then chooses a new password and is logged in.

// resetTokenTTL is how long a reset link works (03 §3.2, §14). It is single use, and a user has one at a time: a
// new link replaces the one before.
const resetTokenTTL = 24 * time.Hour

// Audit actions written here (03 §10).
const (
	auditPasswordResetIssued    = "user.password_reset_issued" // a security event
	auditPasswordResetCompleted = "user.password_reset_completed"
)

// errResetTokenInvalid is 404 reset_token_invalid: the link is unknown, used, replaced or expired.
func errResetTokenInvalid() error { return api.NewError(api.CodeResetTokenInvalid) }

// ---- issue ----

// resetPlan is what IssuePasswordReset found out about its actor and its target in one transaction.
type resetPlan struct {
	who    acting
	target store.User
	// needsPassword: the target is an admin, so an acting admin confirms with the own password. Never for the CLI.
	needsPassword bool
}

// planPasswordReset applies the rules of 03 §7.10 to a reset of user id that a asks for:
//
//  1. the actor: an active admin or the CLI, else forbidden;
//  2. the target → 404 user_not_found;
//  3. an admin can't reset the own account this way → 409 self_action_forbidden: POST /me/password is the place.
//
// The CLI is exempt from 3 (it is nobody's account) and from the password: `isshoni admin users reset-password` is
// how a sole admin who is locked out gets back in.
func planPasswordReset(q *store.Q, a store.Actor, id store.UserID) (resetPlan, error) {
	who, err := actingAdmin(q, a)
	if err != nil {
		return resetPlan{}, err
	}
	target, err := userTarget(q, id)
	if err != nil {
		return resetPlan{}, err
	}
	if actsOnSelf(a, who, target) {
		return resetPlan{}, api.NewError(api.CodeSelfActionForbidden)
	}
	return resetPlan{who: who, target: target,
		needsPassword: a.Kind != store.ActorCLI && target.Role == store.RoleAdmin}, nil
}

// IssuePasswordReset issues a reset link for a user and returns it, Origins.Primary + "/reset#" + token, valid for
// 24 h (POST /api/v1/admin/users/{id}/password-reset; `isshoni admin users reset-password` with store.CLIActor and
// no password; 03 §7.10). The token is in the link only: the database holds its keyed hash, and it is never logged.
// In order:
//
//  1. the actor, the target and the self rule (planPasswordReset) → 403 forbidden, 404 user_not_found, 409
//     self_action_forbidden;
//  2. resetting another admin: the acting admin types the own password again (actorPassword, verifyOwnPassword) →
//     403 wrong_password, also for none; 429 rate_limited; 503 server_busy. The link would hand over that admin
//     account, so a stolen admin cookie alone must not get one;
//  3. one Write, which checks 1 again and that the password is still the one that was verified: the password hash
//     becomes NULL, so every login answers invalid_credentials until the link is completed; every web session and
//     device of the target goes, with their push subscriptions and the device codes the target decided (03 §7.7
//     "Admin: password reset"); the new link's hash replaces the one the user had; user.password_reset_issued is
//     written.
//
// After the commit the target's sessions leave the cache, every connection of the target is closed with
// ReasonPasswordReset, and the admin_password_reset admin alert is raised if the target is an admin (03 §7.11).
//
// A link can be issued for an account of any status. Only an active account can use one: see CheckPasswordReset.
func (s *Service) IssuePasswordReset(ctx context.Context, a store.Actor, id store.UserID, actorPassword string) (Link, error) {
	const op = "issue password reset"
	var plan resetPlan
	err := s.db.Read(ctx, func(q *store.Q) error {
		var err error
		plan, err = planPasswordReset(q, a, id)
		return err
	})
	if err != nil {
		return Link{}, serviceErr(op, err)
	}
	var confirmed *store.User
	if plan.needsPassword {
		if err := s.verifyOwnPassword(ctx, op, plan.who.user, actorPassword, actorMeta(a)); err != nil {
			return Link{}, err
		}
		confirmed = &plan.who.user
	}

	now := s.now()
	expires := now.Add(resetTokenTTL)
	token := newToken(resetTokenBytes)
	var target store.User
	var rv revoked
	err = s.db.Write(ctx, func(q *store.Q) error {
		cur, err := planPasswordReset(q, a, id)
		if err != nil {
			return err
		}
		if cur.needsPassword && !confirmedBy(confirmed, cur.who.user) {
			return errWrongPassword()
		}
		target = cur.target
		if err := q.SetPasswordHash(target.ID, "", now); err != nil {
			return err
		}
		if rv, err = revokePasswordReset.apply(q, target.ID, ""); err != nil {
			return err
		}
		err = q.ReplacePasswordReset(store.PasswordReset{TokenHash: s.keys.inviteTokenHash(token), UserID: target.ID,
			CreatedBy: cur.who.id(), CreatedAt: now, ExpiresAt: expires})
		if err != nil {
			return err
		}
		return q.AppendAudit(store.AuditEntry{At: now, Action: auditPasswordResetIssued, Actor: a,
			TargetKind: targetUser, TargetID: string(target.ID), TargetName: target.Username})
	})
	if err != nil {
		return Link{}, serviceErr(op, err)
	}
	s.revocationCommitted(rv)
	if target.Role == store.RoleAdmin {
		// After the commit (03 §7.11), and whether or not the client is still connected.
		s.alert(context.WithoutCancel(ctx), AdminAlert{Kind: AlertAdminPasswordReset, Actor: a.Name,
			Target: target.Username, At: now})
	}
	return Link{URL: s.origins.Primary + "/reset#" + token, ExpiresAt: expires}, nil
}

// ---- check and complete ----

// resetTokenUser is the rule that the check, the completion and its Write apply to a reset token: the account the
// link resets, when the link is live. A token of the wrong shape (refused without a lookup), an unknown, used,
// replaced or expired one is *api.Error{reset_token_invalid}.
//
// Only an active account can use its link: for a pending one the answer is account_pending and for a disabled one
// account_disabled (403), as a login answers a correct password. Disabling an account deletes its link (03 §7.7),
// so this is a link that was issued for an account that was not active then; it works once the account is
// approved or enabled, while it lasts.
func (s *Service) resetTokenUser(q *store.Q, token string, now time.Time) (store.User, error) {
	if !tokenWellFormed(token, resetTokenBytes) {
		return store.User{}, errResetTokenInvalid()
	}
	link, err := q.PasswordResetByHash(s.keys.inviteTokenHash(token), now)
	if errors.Is(err, store.ErrNotFound) {
		return store.User{}, errResetTokenInvalid()
	}
	if err != nil {
		return store.User{}, err
	}
	user, err := q.UserByID(link.UserID)
	if errors.Is(err, store.ErrNotFound) {
		return store.User{}, errResetTokenInvalid() // the link goes with its account (the cascade): not reached
	}
	if err != nil {
		return store.User{}, err
	}
	if user.Status != store.StatusActive {
		return store.User{}, statusErr(user.Status)
	}
	return user, nil
}

// readResetToken is resetTokenUser in a Read of its own.
func (s *Service) readResetToken(ctx context.Context, op, token string) (store.User, error) {
	now := s.now()
	var user store.User
	err := s.db.Read(ctx, func(q *store.Q) error {
		var err error
		user, err = s.resetTokenUser(q, token, now)
		return err
	})
	if err != nil {
		return store.User{}, serviceErr(op, err)
	}
	return user, nil
}

// CheckPasswordReset answers POST /api/v1/auth/reset/check with the username the link resets, so the page can say
// whose password it is, or that the link is dead, before the form is filled in. It takes a token of the auth-ip
// bucket (429 rate_limited). A dead link is *api.Error{reset_token_invalid}; a live one of an account that is not
// active is account_pending or account_disabled (resetTokenUser).
func (s *Service) CheckPasswordReset(ctx context.Context, token string, m ReqMeta) (username string, err error) {
	if err := s.takeAuthIP(ctx, m); err != nil {
		return "", err
	}
	user, err := s.readResetToken(ctx, "check password reset", token)
	if err != nil {
		return "", err
	}
	return user.Username, nil
}

// CompletePasswordReset sets the new password of the account a reset link is for and logs its holder in
// (POST /api/v1/auth/reset/complete, 03 §7.10). In order:
//
//  1. the auth-ip bucket → 429 rate_limited;
//  2. the token, read-only (resetTokenUser) → 404 reset_token_invalid, 403 account_pending or account_disabled. A
//     request without a live link costs no hash and learns nothing about the password rules;
//  3. the password rules (CheckPassword, with the account's username) → 422 validation_failed {password: code};
//  4. the auth-hash budget, then the hash, outside the transaction → 503 server_busy;
//  5. one Write, which checks 2 again (two requests with one link: the second finds it gone): the hash and
//     password_changed_at are set, the link is deleted, the last-login time is set and a session is opened
//     (openSession: the session of a cookie the request arrived with goes, 03 §7.7), and
//     user.password_reset_completed is written.
//
// The account has no other session to end: issuing the link deleted them all, and nobody could log in since. After
// the commit the session of the old cookie, if there was one, leaves the cache and its connections are closed with
// ReasonSessionRevoked. The returned token is the raw cookie value; httpapi sets it with SetSessionCookie.
func (s *Service) CompletePasswordReset(ctx context.Context, token, password string, m ReqMeta) (LoginResult, error) {
	const op = "complete password reset"
	if err := s.takeAuthIP(ctx, m); err != nil {
		return LoginResult{}, err
	}
	user, err := s.readResetToken(ctx, op, token)
	if err != nil {
		return LoginResult{}, err
	}
	pw, err := newPasswordFor(user, password)
	if err != nil {
		return LoginResult{}, serviceErr(op, err)
	}
	if err := s.takeHashBudget(ctx, m); err != nil {
		return LoginResult{}, err
	}
	phc, err := s.hasher.hash(ctx, pw)
	if err != nil {
		return LoginResult{}, hashErr(op, err)
	}

	now := s.now() // after the hash: the link may have expired while this request waited for a hash slot
	sessionToken := newToken(sessionTokenBytes)
	var ns newSession
	err = s.db.Write(ctx, func(q *store.Q) error {
		cur, err := s.resetTokenUser(q, token, now)
		if err != nil {
			return err
		}
		if cur.UsernameKey != user.UsernameKey {
			// Renamed since the password was checked against the name: the rule "not the username" again.
			if _, err := newPasswordFor(cur, password); err != nil {
				return err
			}
		}
		if err := q.SetPasswordHash(cur.ID, phc, now); err != nil {
			return err
		}
		cur.PasswordHash, cur.PasswordChangedAt, cur.UpdatedAt = phc, now, now
		if err := q.DeletePasswordReset(cur.ID); err != nil {
			return err
		}
		if err := q.TouchLogin(cur.ID, now); err != nil {
			return err
		}
		cur.LastLoginAt = now
		if err := s.openSession(q, cur, sessionToken, m, now, &ns); err != nil {
			return err
		}
		return q.AppendAudit(store.AuditEntry{At: now, Action: auditPasswordResetCompleted, Actor: userActor(cur, m),
			TargetKind: targetUser, TargetID: string(cur.ID), TargetName: cur.Username})
	})
	if err != nil {
		return LoginResult{}, serviceErr(op, err)
	}
	s.sessionOpened(&ns)
	return ns.result, nil
}

// newPasswordFor applies the password rules to the new password of user and returns its normalized form, or 422
// validation_failed {password: code}.
func newPasswordFor(user store.User, password string) (string, error) {
	pw, err := CheckPassword(password, user.UsernameKey)
	if err != nil {
		fields := map[string]string{}
		addFieldError(fields, err)
		if len(fields) == 0 {
			return "", err
		}
		return "", validationFailed(fields)
	}
	return pw, nil
}
