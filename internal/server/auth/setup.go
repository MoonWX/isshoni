package auth

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// First-run setup (03 §7.8): the setup token and the first admin.

// setupTokenTTL is how long a setup link works (03 §3.2, §14). It is single use, and only the newest one works.
const setupTokenTTL = 24 * time.Hour

// Audit actions written here (03 §10).
const (
	//nolint:gosec // G101 false positive: an audit action name, not a credential.
	auditSetupTokenIssued = "setup.token_issued"
	auditSetupCompleted   = "setup.completed"
)

// createdViaSetup is users.created_via of the first admin.
const createdViaSetup = "setup"

// settingServerName is the JSON name of the server.name setting (store.Settings).
const settingServerName = "serverName"

// SetupAvailable is true while no admin row exists, whatever its status. 04's SPA handler serves /setup only while
// it is true and answers 404 afterwards (03 §12.6).
func (s *Service) SetupAvailable(ctx context.Context) (bool, error) {
	var hasAdmin bool
	err := s.db.Read(ctx, func(q *store.Q) error {
		var err error
		hasAdmin, err = q.AnyAdmin()
		return err
	})
	if err != nil {
		return false, internalErr("setup available", err)
	}
	return !hasAdmin, nil
}

// IssueSetupToken replaces every setup token with a new one valid for 24 h and returns its link,
// Origins.Primary + "/setup#" + token; only the newest link works. With an admin present it returns
// *api.Error{setup_unavailable}. One Write deletes the old tokens, stores the new hash and audits
// setup.token_issued. 04's `isshoni setup-url` calls it with store.CLIActor (through the admin socket) and prints
// the URL; the token is never logged.
func (s *Service) IssueSetupToken(ctx context.Context, a store.Actor) (Link, error) {
	now := s.now()
	expires := now.Add(setupTokenTTL)
	token := newToken(setupTokenBytes)
	err := s.db.Write(ctx, func(q *store.Q) error {
		hasAdmin, err := q.AnyAdmin()
		if err != nil {
			return err
		}
		if hasAdmin {
			return api.NewError(api.CodeSetupUnavailable)
		}
		if err := q.ReplaceSetupToken(s.keys.inviteTokenHash(token), now, expires); err != nil {
			return err
		}
		return q.AppendAudit(store.AuditEntry{At: now, Action: auditSetupTokenIssued, Actor: a})
	})
	if err != nil {
		return Link{}, serviceErr("issue setup token", err)
	}
	return Link{URL: s.origins.Primary + "/setup#" + token, ExpiresAt: expires}, nil
}

// CheckSetupToken answers POST /api/v1/auth/setup/check: nil for a live token, so the page can say "link expired"
// before the form is filled in. It takes a token of the auth-ip bucket. With an admin present it returns
// *api.Error{setup_unavailable}; an unknown, replaced or expired token is setup_token_invalid.
func (s *Service) CheckSetupToken(ctx context.Context, token string, m ReqMeta) error {
	if err := s.takeAuthIP(ctx, m); err != nil {
		return err
	}
	return s.checkSetupToken(ctx, token)
}

// checkSetupToken is the read-only token check: setup_unavailable once an admin exists, else setup_token_invalid
// unless the token is live. A token of the wrong shape is refused without a lookup.
func (s *Service) checkSetupToken(ctx context.Context, token string) error {
	now := s.now()
	err := s.db.Read(ctx, func(q *store.Q) error { return s.setupTokenLive(q, token, now) })
	if err != nil {
		return serviceErr("check setup token", err)
	}
	return nil
}

// setupTokenLive is the rule both the check and the completing Write apply.
func (s *Service) setupTokenLive(q *store.Q, token string, now time.Time) error {
	hasAdmin, err := q.AnyAdmin()
	if err != nil {
		return err
	}
	if hasAdmin {
		return api.NewError(api.CodeSetupUnavailable)
	}
	if !tokenWellFormed(token, setupTokenBytes) {
		return api.NewError(api.CodeSetupTokenInvalid)
	}
	ok, err := q.SetupTokenValid(s.keys.inviteTokenHash(token), now)
	if err != nil {
		return err
	}
	if !ok {
		return api.NewError(api.CodeSetupTokenInvalid)
	}
	return nil
}

// CompleteSetup creates the first admin and logs it in (POST /api/v1/auth/setup/complete, 03 §7.8). In order:
//
//  1. the auth-ip bucket → 429 rate_limited;
//  2. the token, read-only → 404 setup_unavailable or setup_token_invalid, so a request without a live token costs
//     no hash (the Write checks again);
//  3. the username and password rules → 422 validation_failed with a code per field;
//  4. the auth-hash budget, then the hash, outside the transaction → 503 server_busy;
//  5. one Write: the token is unexpired and no admin exists; the user (role admin, created_via setup) and its
//     session (openSession); every setup token is deleted; the serverName setting, if one was given
//     (SettingsCache.UpdateTx, which checks it → 422 and writes its own settings.changed row); the default room; the
//     setup.completed row.
//
// Two requests that race leave exactly one admin: the Write of the second finds the admin and answers
// setup_unavailable.
func (s *Service) CompleteSetup(ctx context.Context, in SetupInput, m ReqMeta) (LoginResult, error) {
	if err := s.takeAuthIP(ctx, m); err != nil {
		return LoginResult{}, err
	}
	if err := s.checkSetupToken(ctx, in.Token); err != nil {
		return LoginResult{}, err
	}

	fields := map[string]string{}
	display, key, err := NormalizeUsername(in.Username)
	if err != nil {
		addFieldError(fields, err)
	}
	pw, err := CheckPassword(in.Password, key)
	if err != nil {
		addFieldError(fields, err)
	}
	if len(fields) > 0 {
		return LoginResult{}, validationFailed(fields)
	}

	if err := s.takeHashBudget(ctx, m); err != nil {
		return LoginResult{}, err
	}
	phc, err := s.hasher.hash(ctx, pw)
	if err != nil {
		return LoginResult{}, hashErr("complete setup", err)
	}

	var patch map[string]json.RawMessage
	if in.ServerName != "" {
		raw, err := json.Marshal(in.ServerName)
		if err != nil {
			return LoginResult{}, internalErr("complete setup", err)
		}
		patch = map[string]json.RawMessage{settingServerName: raw}
	}

	now := s.now()
	token := newToken(sessionTokenBytes)
	var ns newSession
	var applySettings func() store.Settings
	err = s.db.Write(ctx, func(q *store.Q) error {
		if err := s.setupTokenLive(q, in.Token, now); err != nil {
			return err
		}
		user := store.User{
			Username:          display,
			UsernameKey:       key,
			PasswordHash:      phc,
			Role:              store.RoleAdmin,
			Status:            store.StatusActive,
			CreatedVia:        createdViaSetup,
			CreatedAt:         now,
			PasswordChangedAt: now,
			LastLoginAt:       now,
		}
		if err := q.CreateUser(&user); err != nil {
			var ce *store.ConflictError
			if errors.As(err, &ce) {
				return api.NewError(api.CodeUsernameTaken)
			}
			return err
		}
		if err := s.openSession(q, user, token, m, now, &ns); err != nil {
			return err
		}
		if err := q.DeleteSetupTokens(); err != nil {
			return err
		}
		actor := userActor(user, m)
		if patch != nil {
			apply, err := s.db.Settings().UpdateTx(q, patch, actor)
			if err != nil {
				return err
			}
			applySettings = apply
		}
		if err := q.EnsureDefaultRoom(now); err != nil {
			return err
		}
		return q.AppendAudit(store.AuditEntry{At: now, Action: auditSetupCompleted, Actor: actor,
			TargetKind: targetUser, TargetID: string(user.ID), TargetName: user.Username})
	})
	if err != nil {
		return LoginResult{}, serviceErr("complete setup", err)
	}
	if applySettings != nil {
		applySettings() // after the commit: swaps the settings cache and runs its OnChange callbacks (03 §9)
	}
	s.sessionOpened(&ns)
	return ns.result, nil
}

// addFieldError records a rule's *FieldError under its field name.
func addFieldError(fields map[string]string, err error) {
	var fe *FieldError
	if errors.As(err, &fe) {
		fields[fe.Field] = fe.Code
	}
}
