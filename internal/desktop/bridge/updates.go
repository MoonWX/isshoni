package bridge

import "context"

// Updates is the bound Updates service (03 §6.2, §16.2) over 06's updater: its status, "Check now" and "Restart and
// update". The updater's wiring in the app is README WA84's; until then every method is not_implemented.
type Updates struct{}

// UpdateStatus is 06 §8.9's Status as the SPA reads it (Updates.Status() and the update.state event): state
// disabled, off, idle, checking, up_to_date, downloading, ready or error; progress 0..1 while downloading;
// checkedAt RFC 3339 (the zero time before the first check); errorCode 06 §8.10's code ("" unless error, or
// disabled: no_keys, not_release).
type UpdateStatus struct {
	State     string  `json:"state"`
	Current   string  `json:"current"`
	Available string  `json:"available"`
	Required  bool    `json:"required"`
	NotesURL  string  `json:"notesUrl"`
	Progress  float64 `json:"progress"`
	CheckedAt string  `json:"checkedAt"`
	ErrorCode string  `json:"errorCode"`
	PolicyOff bool    `json:"policyOff"`
}

// Status returns the updater's status. README WA84.
func (s *Updates) Status() (UpdateStatus, error) {
	return UpdateStatus{}, notImplemented("Updates.Status")
}

// Check checks for an update now (06's CheckNow, at most 45 s): update.<code>. README WA84.
func (s *Updates) Check(ctx context.Context) (UpdateStatus, error) {
	return UpdateStatus{}, notImplemented("Updates.Check")
}

// Install applies the verified installer and quits within 5 s (06's Apply, 03 §11.6): busy while a share is live,
// update.not_ready, update.not_installed. README WA84.
func (s *Updates) Install(ctx context.Context) error { return notImplemented("Updates.Install") }
