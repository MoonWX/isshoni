package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// The runtime settings an admin changes in the UI (03 §9, §12.3 #40–#41, §12.4.8), Admin routes. The values, their
// checks, the pins of the config file and the audit row are store.SettingsCache's; auth.Service.UpdateSettings adds
// the admin alert of a registration-mode change.

// apiSettings mirrors store.Settings into its DTO, field by field (a test checks that none is missing).
func apiSettings(s store.Settings) api.Settings {
	return api.Settings{
		ServerName:             s.ServerName,
		RegistrationMode:       api.RegistrationMode(s.RegistrationMode),
		InviteDefaultTTLHours:  s.InviteDefaultTTLHours,
		InviteDefaultMaxUses:   s.InviteDefaultMaxUses,
		MembersCanInvite:       s.MembersCanInvite,
		MaxParticipantsPerRoom: s.MaxParticipantsPerRoom,
		MaxSharesPerRoom:       s.MaxSharesPerRoom,
		MaxShareBitrateKbps:    s.MaxShareBitrateKbps,
		TransferAlertGB:        s.TransferAlertGB,
		UpdateCheck:            s.UpdateCheck,
		MinClientVersion:       s.MinClientVersion,
		SetupWizardDone:        s.SetupWizardDone,
	}
}

// settingsResponse is the body of both settings endpoints: the settings in force (set), the defaults, and the JSON
// names of the fields the config file pins, which the SPA shows read-only.
func (a *API) settingsResponse(set store.Settings) api.SettingsResponse {
	cache := a.d.DB.Settings()
	return api.SettingsResponse{
		Settings: apiSettings(set),
		Defaults: apiSettings(cache.Defaults()),
		Locked:   cache.Locked(),
	}
}

// getSettings is GET /api/v1/admin/settings (03 §12.3 #40): {settings, defaults, locked}. It reads the cache only.
func (a *API) getSettings(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, a.settingsResponse(a.d.DB.Settings().Get()))
}

// patchSettings is PATCH /api/v1/admin/settings (03 §12.3 #41, §9): a JSON merge, so only the fields present
// change, and the answer is the same shape as GET with the new values. A field is reset by sending its default.
//   - A field listed in locked is 409 setting_locked with params.field, whatever value is sent, also the value in
//     force; it wins over a bad value elsewhere in the patch.
//   - A bad value is 422 validation_failed with a code per field; null is invalid.
//   - Unknown names are ignored (03 §12.1). A body that is not a JSON object is 400 bad_request.
//   - A patch that changes nothing answers 200 and writes nothing: no audit row, no notification.
//
// After a change every admin's SPA refetches the settings, and everyone's refetches me when membersCanInvite
// changed, since it decides a member's permissions.createInvites (03 §12.5). A change of registrationMode raises the
// registration_mode_changed admin alert and is a security event (03 §7.11, §10).
func (a *API) patchSettings(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteError(w, r, api.NewError(api.CodeUnauthenticated))
		return
	}
	var patch map[string]json.RawMessage
	if err := DecodeJSON(w, r, &patch, otherBodyLimit); err != nil {
		WriteError(w, r, err)
		return
	}
	if patch == nil { // the JSON null: one value, but not an object
		WriteError(w, r, api.NewError(api.CodeBadRequest))
		return
	}
	ch, err := a.d.Auth.UpdateSettings(r.Context(), a.actorOf(r, p), patch)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if ch.Before != ch.After {
		a.d.Signal.Notify(NotifyTarget{Admins: true}, protocol.TopicAdminSettings)
		if ch.Before.MembersCanInvite != ch.After.MembersCanInvite {
			a.d.Signal.Notify(NotifyTarget{All: true}, protocol.TopicMe)
		}
	}
	WriteJSON(w, http.StatusOK, a.settingsResponse(ch.After))
}
