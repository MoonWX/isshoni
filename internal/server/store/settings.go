package store

import (
	"context"
	"encoding/json"
	"sync/atomic"
)

// Settings are the runtime settings an admin changes in the UI (03 §9). Only non-default values are stored, as JSON
// under the dotted keys in the comments. The dotted names are also the TOML keys that pin 04's policy settings.
type Settings struct {
	ServerName             string           `json:"serverName"`             // server.name ("" → host of Primary origin)
	RegistrationMode       RegistrationMode `json:"registrationMode"`       // registration.mode
	InviteDefaultTTLHours  int              `json:"inviteDefaultTtlHours"`  // invites.default_ttl_hours
	InviteDefaultMaxUses   int              `json:"inviteDefaultMaxUses"`   // invites.default_max_uses
	MembersCanInvite       bool             `json:"membersCanInvite"`       // invites.members_can_create
	MaxParticipantsPerRoom int              `json:"maxParticipantsPerRoom"` // limits.max_participants_per_room
	MaxSharesPerRoom       int              `json:"maxSharesPerRoom"`       // limits.max_shares_per_room
	MaxShareBitrateKbps    int              `json:"maxShareBitrateKbps"`    // limits.max_bitrate_kbps
	TransferAlertGB        int              `json:"transferAlertGb"`        // limits.transfer_alert_gb
	UpdateCheck            bool             `json:"updateCheck"`            // updates.release_check
	MinClientVersion       string           `json:"minClientVersion"`       // clients.min_version
	SetupWizardDone        bool             `json:"setupWizardDone"`        // setup.wizard_done
}

// defaultSettings is the single source of the policy defaults (03 §9 table). Every limit defaults to 0 (off).
func defaultSettings() Settings {
	return Settings{
		ServerName:             "",
		RegistrationMode:       ModeInvite,
		InviteDefaultTTLHours:  168,
		InviteDefaultMaxUses:   10,
		MembersCanInvite:       false,
		MaxParticipantsPerRoom: 0,
		MaxSharesPerRoom:       0,
		MaxShareBitrateKbps:    0,
		TransferAlertGB:        0,
		UpdateCheck:            true,
		MinClientVersion:       "",
		SetupWizardDone:        false,
	}
}

// SettingsCache holds the current settings for lock-free reads. DB.Settings returns it.
//
// Until the settings slice lands, the cache always holds the defaults: stored values are not loaded yet, and Pin
// and Update return a "not implemented" error.
type SettingsCache struct {
	cur atomic.Pointer[Settings]
}

func newSettingsCache() *SettingsCache {
	c := &SettingsCache{}
	d := defaultSettings()
	c.cur.Store(&d)
	return c
}

// Get returns a snapshot of the current settings without locking.
func (c *SettingsCache) Get() Settings { return *c.cur.Load() }

// Defaults returns the default settings, the single source of policy defaults.
func (c *SettingsCache) Defaults() Settings { return defaultSettings() }

// Locked returns the JSON names of the fields that config pins.
func (c *SettingsCache) Locked() []string { return nil }

// Pin forces a field (by JSON name) to value, as a TOML policy key does. 04 calls it before serving. It validates
// like Update.
func (c *SettingsCache) Pin(field string, value any) error {
	return notImplemented("SettingsCache.Pin")
}

// Update validates and stores a patch (JSON name → value), writes the settings.changed audit row in the same
// transaction, then swaps the cache and runs the OnChange callbacks.
func (c *SettingsCache) Update(ctx context.Context, patch map[string]json.RawMessage, a Actor) (Settings, error) {
	return Settings{}, notImplemented("SettingsCache.Update")
}

// OnChange registers fn to run after every committed change with the old and new settings. cancel removes it.
func (c *SettingsCache) OnChange(fn func(prev, next Settings)) (cancel func()) {
	// Nothing changes the settings yet, so fn never runs.
	return func() {}
}
