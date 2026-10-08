package auth

import (
	"context"
	"encoding/json"

	"github.com/MoonWX/isshoni/internal/server/store"
)

// The settings patch of an admin (03 §9), with the admin alert of 03 §7.11.

// SettingsChange is the outcome of UpdateSettings: the effective settings before and after the patch. They are
// equal when the patch changed nothing.
type SettingsChange struct{ Before, After store.Settings }

// UpdateSettings applies an admin's settings patch (PATCH /api/v1/admin/settings, 03 §9; JSON name → JSON value,
// only the fields present change) and raises the registration_mode_changed admin alert when it changed
// registrationMode (03 §7.11), with the actor's name. a is an admin or store.CLIActor (actingAdmin).
//
// The patch is store.SettingsCache.UpdateTx's: a pinned field in it is *api.Error{setting_locked} with params.field
// (409), whatever value is sent; a bad value, null too, is *api.Error{validation_failed} with a code per field
// (422); unknown names are ignored. One Write reads the actor's account and writes the changed fields and the
// settings.changed audit row (a row that changes registrationMode is a security event, 03 §10); after the commit the
// cache swaps and its OnChange callbacks run. A patch that changes nothing writes nothing and raises no alert. An
// actor who is not an admin is forbidden before the patch is looked at.
//
// It is an addition to the API of 03 §7.13, where the settings have no service call: the alert needs the actor,
// which the cache's OnChange callbacks don't get, and only auth holds the AdminAlerter. Calls are serialized, so
// Before and After are exactly this call's change, also when two admins save at once.
func (s *Service) UpdateSettings(ctx context.Context, a store.Actor, patch map[string]json.RawMessage) (SettingsChange, error) {
	ch, err := s.patchSettings(ctx, a, patch)
	if err != nil {
		return SettingsChange{}, serviceErr("update settings", err)
	}
	if ch.Before.RegistrationMode != ch.After.RegistrationMode {
		// After the commit (03 §7.11), and whether or not the client is still connected.
		s.alert(context.WithoutCancel(ctx), AdminAlert{Kind: AlertRegistrationModeChanged, Actor: a.Name, At: s.now()})
	}
	return ch, nil
}

// patchSettings runs one settings update at a time and returns the settings around it. The actor is checked in the
// Write that changes the settings, like in every other admin call: an admin who was demoted or disabled a moment
// ago changes nothing. No other path changes a setting an admin can patch while the server serves: setup/complete
// sets only the server name, before any admin exists, and config pins its fields before the listeners start.
func (s *Service) patchSettings(ctx context.Context, a store.Actor, patch map[string]json.RawMessage) (SettingsChange, error) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	cache := s.db.Settings()
	before := cache.Get()
	var apply func() store.Settings
	err := s.db.Write(ctx, func(q *store.Q) error {
		if _, err := actingAdmin(q, a); err != nil {
			return err
		}
		var err error
		apply, err = cache.UpdateTx(q, patch, a)
		return err
	})
	if err != nil {
		return SettingsChange{}, err
	}
	// After the commit: apply swaps the cache and runs its OnChange callbacks (03 §9).
	return SettingsChange{Before: before, After: apply()}, nil
}
