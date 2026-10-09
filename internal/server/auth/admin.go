package auth

import (
	"context"
	"errors"
	"net/netip"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// Admin users (03 §7.11, §12.4.8; README S64): what an admin, or the operator's CLI through the admin socket, does
// to an account. Renaming it, changing its role, disabling and enabling it (UpdateUser), deleting it (DeleteUser)
// and signing it out everywhere (SignOutUser), with the last-admin rule, the self rules and the password an admin
// types again to make another admin. The reset links an admin issues are in reset.go.
//
// Every call checks its actor inside its own Write (actingAdmin): an active admin or store.CLIActor, as the
// accounts are at that moment. The calls that take credentials away are rows of the revocation table (revoke.go).

// Audit actions written here (03 §10). user.deleted is the one of selfservice.go, with {self: false}.
const (
	auditUserRenamed     = "user.renamed"      // detail {from, to}
	auditUserRoleChanged = "user.role_changed" // detail {from, to}; a security event
	auditUserDisabled    = "user.disabled"     // a security event
	auditUserEnabled     = "user.enabled"
	auditUserSignedOut   = "user.signed_out" // detail {sessions, devices}
)

// JSON names of the fields of PATCH /api/v1/admin/users/{id}, the keys of its validation_failed answer (the
// username rules name theirs "username" already).
const (
	fieldRole   = "role"
	fieldStatus = "status"
)

// userTarget is the account an admin call is about: *api.Error{user_not_found} for an ID nobody has.
func userTarget(q *store.Q, id store.UserID) (store.User, error) {
	u, err := q.UserByID(id)
	if errors.Is(err, store.ErrNotFound) {
		return store.User{}, api.NewError(api.CodeUserNotFound)
	}
	return u, err
}

// actsOnSelf reports whether the actor is the account the call is about. The CLI is nobody's account.
func actsOnSelf(a store.Actor, who acting, target store.User) bool {
	return a.Kind == store.ActorUser && who.user.ID == target.ID
}

// actorMeta is the ReqMeta of an admin call for the one check that needs one, the acting admin's own password
// (verifyOwnPassword keys its buckets by the address): the IP the actor acts from, the zero Addr when unknown.
func actorMeta(a store.Actor) ReqMeta {
	ip, err := netip.ParseAddr(a.IP)
	if err != nil {
		return ReqMeta{}
	}
	return ReqMeta{IP: ip}
}

// confirmedBy reports whether the password that the acting admin typed for a change still stands when the change
// is written. checked is the actor as it was read when the password was verified (nil: no password was asked
// for), current the actor as the Write reads it. A change that needs the password and was not verified against the
// hash the account has now is answered wrong_password: the need arose after the first read (the target became an
// admin in between, say), or the actor's password changed since.
func confirmedBy(checked *store.User, current store.User) bool {
	return checked != nil && checked.ID == current.ID && checked.PasswordHash == current.PasswordHash
}

// ---- rename, role, status ----

// userPlan is what an UpdateUser call does to its target, worked out from the accounts as one transaction sees
// them. UpdateUser plans twice: in a read, to answer what it can before any hashing, and again in its Write, which
// applies the plan it made there.
type userPlan struct {
	who    acting
	target store.User
	// rename says the display name changes, to username with the comparison key usernameKey.
	rename                bool
	username, usernameKey string
	// role and status are the values after the change: the target's own where nothing changes.
	role   store.Role
	status store.UserStatus
	// needsPassword: the change makes the target an admin, which an acting admin confirms with the own password
	// (03 §7.11). Never for the CLI.
	needsPassword bool
}

func (p userPlan) roleChanges() bool   { return p.role != p.target.Role }
func (p userPlan) statusChanges() bool { return p.status != p.target.Status }
func (p userPlan) changes() bool       { return p.rename || p.roleChanges() || p.statusChanges() }

// activeAdmin reports whether an account with this role and status counts for the last-admin rule.
func activeAdmin(role store.Role, status store.UserStatus) bool {
	return role == store.RoleAdmin && status == store.StatusActive
}

// planUserChange works out what ch does to user id when a asks for it, and refuses what the rules of 03 §7.11 and
// §12.4.8 refuse, in this order:
//
//  1. the actor: an active admin or the CLI, else forbidden;
//  2. the values, before the target is looked at → 422 validation_failed with a code per field: the username rules
//     (NormalizeUsername), a role that is neither admin nor user (invalid), a status that is neither active nor
//     disabled (not_allowed: pending is where a sign-up starts, never a value to set);
//  3. the target → 404 user_not_found;
//  4. a status for a pending sign-up → 422 {status: not_allowed}: it is approved or rejected through the approval
//     queue, which records who decided;
//  5. disabling oneself → 409 self_action_forbidden;
//  6. the last-admin rule: the target is an active admin and would not be one afterwards (demoted, disabled, or
//     both), and no other active admin exists → 409 last_admin. Demoting oneself is allowed while another one
//     does;
//  7. a new name that another account has, a pending sign-up too → 409 username_taken.
//
// A value the target has already is no change. Another spelling of the own name (its case) is a rename.
func planUserChange(q *store.Q, a store.Actor, id store.UserID, ch UserChange) (userPlan, error) {
	who, err := actingAdmin(q, a)
	if err != nil {
		return userPlan{}, err
	}

	fields := map[string]string{}
	var display, key string
	if ch.Username != nil {
		if display, key, err = NormalizeUsername(*ch.Username); err != nil {
			addFieldError(fields, err)
		}
	}
	if ch.Role != nil && *ch.Role != store.RoleAdmin && *ch.Role != store.RoleUser {
		fields[fieldRole] = FieldInvalid
	}
	if ch.Status != nil && *ch.Status != store.StatusActive && *ch.Status != store.StatusDisabled {
		fields[fieldStatus] = api.FieldNotAllowed
	}
	if len(fields) > 0 {
		return userPlan{}, validationFailed(fields)
	}

	target, err := userTarget(q, id)
	if err != nil {
		return userPlan{}, err
	}
	p := userPlan{who: who, target: target, role: target.Role, status: target.Status}
	if ch.Status != nil {
		if target.Status == store.StatusPending {
			return userPlan{}, validationFailed(map[string]string{fieldStatus: api.FieldNotAllowed})
		}
		p.status = *ch.Status
	}
	if ch.Role != nil {
		p.role = *ch.Role
	}
	if ch.Username != nil && display != target.Username {
		p.rename, p.username, p.usernameKey = true, display, key
	}

	if actsOnSelf(a, who, target) && p.statusChanges() && p.status == store.StatusDisabled {
		return userPlan{}, api.NewError(api.CodeSelfActionForbidden)
	}
	if activeAdmin(target.Role, target.Status) && !activeAdmin(p.role, p.status) {
		if err := refuseLastAdmin(q, target); err != nil {
			return userPlan{}, err
		}
	}
	if p.rename && p.usernameKey != target.UsernameKey {
		switch _, err := q.UserByUsernameKey(p.usernameKey); {
		case err == nil:
			return userPlan{}, api.NewError(api.CodeUsernameTaken)
		case !errors.Is(err, store.ErrNotFound):
			return userPlan{}, err
		}
	}
	p.needsPassword = a.Kind != store.ActorCLI && p.roleChanges() && p.role == store.RoleAdmin
	return p, nil
}

// UpdateUser renames a user, changes its role or its status, or any of the three at once
// (PATCH /api/v1/admin/users/{id}, 03 §7.11, §12.4.8; `isshoni admin users set-role|disable|enable` with
// store.CLIActor). nil fields of ch stay as they are, and so does a field that is given the value it has: a call
// that changes nothing writes nothing and returns the account. In order:
//
//  1. the actor, the values, the target and the rules (planUserChange) → 403 forbidden, 422 validation_failed, 404
//     user_not_found, 409 self_action_forbidden, last_admin or username_taken. All of them cost no hash;
//  2. granting admin: an acting admin types the own password again (ch.ActorPassword, verifyOwnPassword) → 403
//     wrong_password, also for none; 429 rate_limited; 503 server_busy. A stolen admin cookie alone can't mint a
//     second admin. The CLI is exempt: the admin socket has checked its peer (04 §12.1);
//  3. one Write, which plans again on the rows as they are then and checks that the password is still the one
//     that was verified:
//     - a rename → user.renamed {from, to};
//     - a role change → user.role_changed {from, to};
//     - disabling → the account's web sessions, devices and pending reset link go (03 §7.7 "Admin: disable user")
//     and user.disabled is written; enabling → user.enabled. An enabled account has no session and no reset
//     link: it logs in with its password, or an admin issues a link.
//
// After the commit the user's sessions leave the session cache, so the next request of each sees the new name and
// role (a demoted admin gets 403 on the next admin call) and a disabled account is let in nowhere. Disabling closes
// every connection of the user with ReasonAccountDisabled. A rename or a role change closes none: over REST the hub
// is told by httpapi (Signal.UserChanged); a change from the CLI reaches open connections at the hub's next
// Revalidate (03 §7.7). A role change raises the admin_granted or admin_revoked admin alert (03 §7.11).
//
// The returned account is the row after the Write.
func (s *Service) UpdateUser(ctx context.Context, a store.Actor, id store.UserID, ch UserChange) (store.User, error) {
	const op = "update user"
	var plan userPlan
	err := s.db.Read(ctx, func(q *store.Q) error {
		var err error
		plan, err = planUserChange(q, a, id, ch)
		return err
	})
	if err != nil {
		return store.User{}, serviceErr(op, err)
	}
	if !plan.changes() {
		return plan.target, nil
	}
	var confirmed *store.User
	if plan.needsPassword {
		if err := s.verifyOwnPassword(ctx, op, plan.who.user, ch.ActorPassword, actorMeta(a)); err != nil {
			return store.User{}, err
		}
		confirmed = &plan.who.user
	}

	now := s.now()
	var done userPlan
	var user store.User
	var rv revoked
	err = s.db.Write(ctx, func(q *store.Q) error {
		cur, err := planUserChange(q, a, id, ch)
		if err != nil {
			return err
		}
		if cur.needsPassword && !confirmedBy(confirmed, cur.who.user) {
			return errWrongPassword()
		}
		done, user = cur, cur.target
		if !cur.changes() {
			return nil // somebody made the same change between the read and this Write
		}
		// user is the account as the rows written so far leave it: after a rename they carry the new name.
		entry := func(action string, detail map[string]any) store.AuditEntry {
			return store.AuditEntry{At: now, Action: action, Actor: a, TargetKind: targetUser,
				TargetID: string(user.ID), TargetName: user.Username, Detail: detail}
		}
		if cur.rename {
			if err := q.RenameUser(id, cur.username, cur.usernameKey, now); err != nil {
				return usernameTaken(err)
			}
			from := user.Username
			user.Username = cur.username
			if err := q.AppendAudit(entry(auditUserRenamed, map[string]any{"from": from, "to": cur.username})); err != nil {
				return err
			}
		}
		if cur.roleChanges() {
			if err := q.SetRole(id, cur.role, now); err != nil {
				return err
			}
			detail := map[string]any{"from": string(cur.target.Role), "to": string(cur.role)}
			if err := q.AppendAudit(entry(auditUserRoleChanged, detail)); err != nil {
				return err
			}
		}
		if cur.statusChanges() {
			if err := q.SetStatus(id, cur.status, now); err != nil {
				return err
			}
			action := auditUserEnabled
			if cur.status == store.StatusDisabled {
				action = auditUserDisabled
				if rv, err = revokeAccountDisabled.apply(q, id, ""); err != nil {
					return err
				}
			}
			if err := q.AppendAudit(entry(action, nil)); err != nil {
				return err
			}
		}
		user, err = q.UserByID(id)
		return err
	})
	if err != nil {
		return store.User{}, serviceErr(op, err)
	}
	if !done.changes() {
		return user, nil
	}

	// After the commit (03 §7.4, §7.7): the cache first, then the connections, then the alert.
	if done.statusChanges() && done.status == store.StatusDisabled {
		s.revocationCommitted(rv)
	} else {
		s.sessions.invalidateUser(id)
	}
	if done.roleChanges() {
		kind := AlertAdminRevoked
		if done.role == store.RoleAdmin {
			kind = AlertAdminGranted
		}
		// It does not depend on the client staying connected.
		s.alert(context.WithoutCancel(ctx), AdminAlert{Kind: kind, Actor: a.Name, Target: user.Username, At: now})
	}
	return user, nil
}

// ---- delete, sign out ----

// DeleteUser deletes an account (DELETE /api/v1/admin/users/{id}; 03 §7.7 "Delete user", §7.11). In one Write:
//
//  1. the actor: an active admin or the CLI, else forbidden;
//  2. the target → 404 user_not_found. A pending sign-up can be deleted here too; rejecting it does the same and
//     says so in the audit log;
//  3. deleting oneself → 409 self_action_forbidden: POST /me/delete is the place, and asks for the password;
//  4. the last-admin rule → 409 last_admin;
//  5. the users row is deleted, and its sessions, devices, push subscriptions and preferences and reset link go
//     with it (the cascade); invites and rooms the user created stay, without a creator. user.deleted {self: false}
//     is written; the row keeps the username as a snapshot.
//
// After the commit the user's sessions leave the cache and every connection of the user is closed with
// ReasonAccountDeleted. The username is free again.
func (s *Service) DeleteUser(ctx context.Context, a store.Actor, id store.UserID) error {
	now := s.now()
	err := s.db.Write(ctx, func(q *store.Q) error {
		who, err := actingAdmin(q, a)
		if err != nil {
			return err
		}
		target, err := userTarget(q, id)
		if err != nil {
			return err
		}
		if actsOnSelf(a, who, target) {
			return api.NewError(api.CodeSelfActionForbidden)
		}
		if err := refuseLastAdmin(q, target); err != nil {
			return err
		}
		if err := q.DeleteUser(target.ID); err != nil {
			return err
		}
		return q.AppendAudit(store.AuditEntry{At: now, Action: auditUserDeleted, Actor: a,
			TargetKind: targetUser, TargetID: string(target.ID), TargetName: target.Username,
			Detail: map[string]any{"self": false}})
	})
	if err != nil {
		return serviceErr("delete user", err)
	}
	s.revocationCommitted(revokeAccountDeleted.deleted(id))
	return nil
}

// SignOutUser deletes every web session and every device of a user and returns how many of each there were
// (POST /api/v1/admin/users/{id}/sign-out; 03 §7.7 "Admin: sign out everywhere"). Their push subscriptions and the
// device codes the user decided go with them; the password and a pending reset link stay. It writes
// user.signed_out {sessions, devices}. After the commit the user's sessions leave the cache and every connection of
// the user is closed with ReasonSessionRevoked. An unknown user is *api.Error{user_not_found}.
//
// An admin may sign the own account out this way: the session the request came with goes like the others. The
// counts are the deleted rows, the ones the admin's user list counts: an expired session the janitor has not pruned
// yet is one of them.
func (s *Service) SignOutUser(ctx context.Context, a store.Actor, id store.UserID) (sessions, devices int, err error) {
	now := s.now()
	var rv revoked
	err = s.db.Write(ctx, func(q *store.Q) error {
		if _, err := actingAdmin(q, a); err != nil {
			return err
		}
		target, err := userTarget(q, id)
		if err != nil {
			return err
		}
		if rv, err = revokeSignedOut.apply(q, target.ID, ""); err != nil {
			return err
		}
		return q.AppendAudit(store.AuditEntry{At: now, Action: auditUserSignedOut, Actor: a,
			TargetKind: targetUser, TargetID: string(target.ID), TargetName: target.Username,
			Detail: map[string]any{"sessions": len(rv.sessions), "devices": len(rv.devices)}})
	})
	if err != nil {
		return 0, 0, serviceErr("sign out user", err)
	}
	s.revocationCommitted(rv)
	return len(rv.sessions), len(rv.devices), nil
}

// ---- lookups ----

// UserByUsername looks an account up by any spelling of its username: the comparison key a login uses (loginKey),
// so case and width don't matter and no shape rule applies. The admin socket's commands take a name where REST
// takes an ID (03 §12.6). A name no account has, one that no account can have included, is
// *api.Error{user_not_found}.
func (s *Service) UserByUsername(ctx context.Context, username string) (store.User, error) {
	key, isKey := loginKey(username)
	if !isKey {
		return store.User{}, api.NewError(api.CodeUserNotFound)
	}
	var user store.User
	err := s.db.Read(ctx, func(q *store.Q) error {
		var err error
		user, err = q.UserByUsernameKey(key)
		if errors.Is(err, store.ErrNotFound) {
			return api.NewError(api.CodeUserNotFound)
		}
		return err
	})
	if err != nil {
		return store.User{}, serviceErr("user by username", err)
	}
	return user, nil
}
