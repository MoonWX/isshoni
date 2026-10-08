package auth

import (
	"context"
	"errors"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// Registration and the approval queue (03 §7.9): Register in its three modes, and Approve, Reject and RejectAll for
// the pending sign-ups of the approval mode.

// maxPendingSignups caps the approval queue (03 §7.9, §14): a flood of fake sign-ups stops there.
const maxPendingSignups = 50

// users.created_via of the accounts made here.
const (
	createdViaInvite = "invite"
	createdViaSignup = "signup"
)

// Audit actions written here (03 §10).
const (
	auditUserRegistered  = "user.registered"
	auditSignupRequested = "user.signup_requested"
	auditUserApproved    = "user.approved"
	auditSignupRejected  = "user.signup_rejected"
)

// registrationOpen is the mode rule of 03 §7.9 for a registration with or without an invite:
//
//	mode      with an invite                     without one
//	invite    works                              403 invite_required
//	approval  works (the account is active)      a pending sign-up
//	closed    403 registration_closed            403 registration_closed
func registrationOpen(mode store.RegistrationMode, withInvite bool) error {
	switch {
	case mode == store.ModeClosed:
		return errRegistrationClosed()
	case !withInvite && mode != store.ModeApproval:
		return api.NewError(api.CodeInviteRequired)
	}
	return nil
}

// takeRegisterIP is the register-ip bucket (03 §7.3): a sign-up request without an invite takes a token, after
// auth-ip and before any other work. It is what limits name probing in the approval mode to 5 per hour per address.
// Registrations with an invite don't take one: the invite's own maxUses limits them. The first block of an episode
// writes an auth.throttled {scope: ip} row.
func (s *Service) takeRegisterIP(ctx context.Context, m ReqMeta) error {
	v := s.throttle.registerIP.take(IPKey(m.IP))
	if v.OK {
		return nil
	}
	if v.First {
		s.auditAlone(ctx, store.AuditEntry{At: s.now(), Action: auditThrottled, Outcome: outcomeDenied,
			Actor: anonymous(m), Detail: map[string]any{"scope": scopeIP, "key": ipKeyText(m.IP)}})
	}
	s.logBlocked(ctx, m, "register-ip")
	return rateLimited(v)
}

// usernameTaken turns the store's unique-constraint error of CreateUser into 409 username_taken.
func usernameTaken(err error) error {
	var ce *store.ConflictError
	if errors.As(err, &ce) {
		return api.NewError(api.CodeUsernameTaken)
	}
	return err
}

// Register creates an account (POST /api/v1/auth/register, 03 §7.9): with an invite token an active one that is
// logged in at once, without one a pending sign-up that an admin approves or rejects. In order:
//
//  1. the auth-ip bucket, then, only without an invite, register-ip → 429 rate_limited;
//  2. the mode (registrationOpen) → 403 registration_closed or invite_required;
//  3. with an invite: its state, read-only and without using it → 404 invite_invalid, 410 invite_revoked,
//     invite_expired or invite_used_up;
//  4. the username and password rules → 422 validation_failed with a code per field;
//  5. without an invite: a full approval queue (50 pending sign-ups) → 409 limit_reached {limit: pending_signups};
//  6. a username that is taken, by a pending sign-up too → 409 username_taken. It comes after the invite, so names
//     can't be probed without one; in the approval mode anyone can probe, at the pace of register-ip;
//  7. the auth-hash budget, then the hash, outside the transaction → 503 server_busy;
//  8. one Write, which checks 2, 3, 5 and 6 again:
//     - with an invite: the use is counted (atomic, so parallel registrations never pass maxUses), the user is
//     created active with created_via invite and the invite's ID, a session is opened (openSession: the session of
//     a cookie the request arrived with goes) and user.registered {inviteId} is written. A name that was taken in
//     the meantime rolls the use back;
//     - without one: the user is created pending with created_via signup, and user.signup_requested is written with
//     the sign-up's IP. No session: the account can't log in before an admin approves it. After the commit the
//     signup_pending admin alert is raised, at most once per 10 minutes.
//
// Steps 3, 5 and 6 share one read, so a request that will be refused costs no hash.
func (s *Service) Register(ctx context.Context, in RegisterInput, m ReqMeta) (RegisterResult, error) {
	if err := s.takeAuthIP(ctx, m); err != nil {
		return RegisterResult{}, err
	}
	withInvite := in.InviteToken != ""
	if !withInvite {
		if err := s.takeRegisterIP(ctx, m); err != nil {
			return RegisterResult{}, err
		}
	}
	if err := registrationOpen(s.db.Settings().Get().RegistrationMode, withInvite); err != nil {
		return RegisterResult{}, err
	}

	// The field rules are pure; their verdict waits for the invite's (step 3 comes before step 4).
	fields := map[string]string{}
	display, key, err := NormalizeUsername(in.Username)
	if err != nil {
		addFieldError(fields, err)
	}
	pw, err := CheckPassword(in.Password, key)
	if err != nil {
		addFieldError(fields, err)
	}

	now := s.now()
	var taken, queueFull bool
	err = s.db.Read(ctx, func(q *store.Q) error {
		if withInvite {
			if _, err := s.liveInvite(q, in.InviteToken, now); err != nil {
				return err
			}
		} else {
			n, err := q.CountPending()
			if err != nil {
				return err
			}
			queueFull = n >= maxPendingSignups
		}
		if key == "" {
			return nil // no valid username to look up
		}
		switch _, err := q.UserByUsernameKey(key); {
		case err == nil:
			taken = true
		case !errors.Is(err, store.ErrNotFound):
			return err
		}
		return nil
	})
	switch {
	case err != nil:
		return RegisterResult{}, serviceErr("register", err)
	case len(fields) > 0:
		return RegisterResult{}, validationFailed(fields)
	case queueFull:
		return RegisterResult{}, limitReached(api.LimitKindPendingSignups)
	case taken:
		return RegisterResult{}, api.NewError(api.CodeUsernameTaken)
	}

	if err := s.takeHashBudget(ctx, m); err != nil {
		return RegisterResult{}, err
	}
	phc, err := s.hasher.hash(ctx, pw)
	if err != nil {
		return RegisterResult{}, hashErr("register", err)
	}

	user := store.User{
		Username:     display,
		UsernameKey:  key,
		PasswordHash: phc,
		Role:         store.RoleUser,
	}
	if withInvite {
		return s.registerWithInvite(ctx, in.InviteToken, user, m)
	}
	return s.requestSignup(ctx, user, m)
}

// registerWithInvite is the Write of a registration with an invite, and its after-commit steps.
func (s *Service) registerWithInvite(ctx context.Context, inviteToken string, user store.User, m ReqMeta) (RegisterResult, error) {
	now := s.now() // after the hash: the invite may have expired while this request waited for a hash slot
	token := newToken(sessionTokenBytes)
	var ns newSession
	err := s.db.Write(ctx, func(q *store.Q) error {
		if err := registrationOpen(s.db.Settings().Get().RegistrationMode, true); err != nil {
			return err
		}
		inv, err := s.liveInvite(q, inviteToken, now)
		if err != nil {
			return err
		}
		if err := q.UseInvite(inv.ID, now); err != nil {
			if errors.Is(err, store.ErrInviteUnusable) {
				// liveInvite just accepted it in this transaction, so this is not reached; answer like the check.
				return api.NewError(api.CodeInviteInvalid)
			}
			return err
		}
		user.Status = store.StatusActive
		user.CreatedVia = createdViaInvite
		user.InviteID = inv.ID
		user.CreatedAt, user.PasswordChangedAt, user.LastLoginAt = now, now, now
		if err := q.CreateUser(&user); err != nil {
			return usernameTaken(err)
		}
		if err := s.openSession(q, user, token, m, now, &ns); err != nil {
			return err
		}
		return q.AppendAudit(store.AuditEntry{At: now, Action: auditUserRegistered, Actor: userActor(user, m),
			TargetKind: targetUser, TargetID: string(user.ID), TargetName: user.Username,
			Detail: map[string]any{"inviteId": string(inv.ID)}})
	})
	if err != nil {
		return RegisterResult{}, serviceErr("register", err)
	}
	s.sessionOpened(&ns)
	res := ns.result
	return RegisterResult{Login: &res}, nil
}

// requestSignup is the Write of a sign-up without an invite (the approval mode), and the signup_pending alert.
func (s *Service) requestSignup(ctx context.Context, user store.User, m ReqMeta) (RegisterResult, error) {
	now := s.now()
	err := s.db.Write(ctx, func(q *store.Q) error {
		if err := registrationOpen(s.db.Settings().Get().RegistrationMode, false); err != nil {
			return err
		}
		n, err := q.CountPending()
		if err != nil {
			return err
		}
		if n >= maxPendingSignups {
			return limitReached(api.LimitKindPendingSignups)
		}
		user.Status = store.StatusPending
		user.CreatedVia = createdViaSignup
		user.CreatedAt, user.PasswordChangedAt = now, now
		if err := q.CreateUser(&user); err != nil {
			return usernameTaken(err)
		}
		// Nobody is logged in: the actor is anonymous, with the name asked for and the sign-up's IP, which the
		// approval queue shows (03 §10).
		return q.AppendAudit(store.AuditEntry{At: now, Action: auditSignupRequested,
			Actor:      store.Actor{Kind: store.ActorAnonymous, Name: user.Username, IP: ipString(m.IP)},
			TargetKind: targetUser, TargetID: string(user.ID), TargetName: user.Username})
	})
	if err != nil {
		return RegisterResult{}, serviceErr("register", err)
	}
	// After the commit (03 §7.11). The sign-ups in between show in the queue the alert links to. The alert does not
	// depend on the client staying connected.
	if s.throttle.signupAlert.take(struct{}{}).OK {
		s.alert(context.WithoutCancel(ctx), AdminAlert{Kind: AlertSignupPending, Actor: store.SystemActor.Name,
			Target: user.Username, At: now})
	}
	return RegisterResult{Pending: true}, nil
}

// ---- the approval queue ----

// Approve activates a pending sign-up (POST /api/v1/admin/approvals/{id}/approve, 03 §7.9): status active,
// approved_by and approved_at are set, and user.approved is written. From then on the user can log in; before,
// a login answered 403 account_pending. An unknown user, or one that is not pending, is
// *api.Error{user_not_found}. a is an admin or store.CLIActor (actingAdmin).
func (s *Service) Approve(ctx context.Context, a store.Actor, id store.UserID) (store.User, error) {
	now := s.now()
	var user store.User
	err := s.db.Write(ctx, func(q *store.Q) error {
		who, err := actingAdmin(q, a)
		if err != nil {
			return err
		}
		if err := q.Approve(id, who.id(), now); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return api.NewError(api.CodeUserNotFound)
			}
			return err
		}
		if user, err = q.UserByID(id); err != nil {
			return err
		}
		return q.AppendAudit(store.AuditEntry{At: now, Action: auditUserApproved, Actor: a,
			TargetKind: targetUser, TargetID: string(user.ID), TargetName: user.Username})
	})
	if err != nil {
		return store.User{}, serviceErr("approve", err)
	}
	// Every status change invalidates the session cache (03 §7.4). A pending user has no session and no connection
	// yet, so there is nothing to close.
	s.sessions.invalidateUser(id)
	return user, nil
}

// Reject deletes a pending sign-up, which frees its username, and writes user.signup_rejected (03 §7.9). An unknown
// user, or one that is not pending, is *api.Error{user_not_found}: other accounts are deleted through DeleteUser,
// with its rules. a is an admin or store.CLIActor (actingAdmin).
func (s *Service) Reject(ctx context.Context, a store.Actor, id store.UserID) error {
	now := s.now()
	err := s.db.Write(ctx, func(q *store.Q) error {
		if _, err := actingAdmin(q, a); err != nil {
			return err
		}
		user, err := q.UserByID(id)
		if errors.Is(err, store.ErrNotFound) || (err == nil && user.Status != store.StatusPending) {
			return api.NewError(api.CodeUserNotFound)
		}
		if err != nil {
			return err
		}
		if err := q.DeleteUser(id); err != nil {
			return err
		}
		return q.AppendAudit(store.AuditEntry{At: now, Action: auditSignupRejected, Actor: a,
			TargetKind: targetUser, TargetID: string(user.ID), TargetName: user.Username})
	})
	if err != nil {
		return serviceErr("reject", err)
	}
	s.sessions.invalidateUser(id)
	return nil
}

// RejectAll deletes every sign-up that is pending when its transaction runs and returns how many there were
// (03 §7.9): the answer to a flood of fake sign-ups. A real sign-up that arrived a moment before goes too; its
// username is free again, so that friend signs up once more. It writes one user.signup_rejected {all: true, count}
// row without a target; an empty queue changes nothing and writes no row. a is an admin or store.CLIActor
// (actingAdmin).
func (s *Service) RejectAll(ctx context.Context, a store.Actor) (int, error) {
	now := s.now()
	var ids []store.UserID
	err := s.db.Write(ctx, func(q *store.Q) error {
		if _, err := actingAdmin(q, a); err != nil {
			return err
		}
		var err error
		if ids, err = q.DeletePending(); err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		return q.AppendAudit(store.AuditEntry{At: now, Action: auditSignupRejected, Actor: a,
			Detail: map[string]any{"all": true, "count": len(ids)}})
	})
	if err != nil {
		return 0, serviceErr("reject all", err)
	}
	for _, id := range ids {
		s.sessions.invalidateUser(id)
	}
	return len(ids), nil
}
