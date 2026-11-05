package bridge

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/MoonWX/isshoni/internal/desktop/app"
	"github.com/MoonWX/isshoni/internal/desktop/bridge/codes"
	"github.com/MoonWX/isshoni/internal/update"
)

// Updates is the bound Updates service (03 §6.2, §16.2) over the app's updater (06's): its status, "Check now" and
// "Restart and update". The update.state event carries every status change (bridge.New subscribes).
type Updates struct {
	ctl UpdateControl // nil: not_implemented
	log *slog.Logger
}

// UpdateControl is what Updates calls of the app (internal/desktop/app's updates.go); *app.App implements it. Its
// methods answer app.ErrNotImplemented while the app has no updater.
type UpdateControl interface {
	UpdateStatus() (update.Status, error)
	CheckUpdate(ctx context.Context) (update.Status, error)
	// InstallUpdate is app.ErrBusy while a share is live and app.ErrUpdateNotReady without a verified installer;
	// once the installer runs, the app quits.
	InstallUpdate(ctx context.Context) error
	SubscribeUpdates(fn func(update.Status)) (cancel func())
}

var _ UpdateControl = (*app.App)(nil)

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

// statusView maps 06's Status.
func statusView(st update.Status) UpdateStatus {
	return UpdateStatus{
		State:     string(st.State),
		Current:   st.Current,
		Available: st.Available,
		Required:  st.Required,
		NotesURL:  st.NotesURL,
		Progress:  st.Progress,
		CheckedAt: st.CheckedAt.UTC().Format(time.RFC3339),
		ErrorCode: st.ErrorCode,
		PolicyOff: st.PolicyOff,
	}
}

// Status returns the updater's status.
func (s *Updates) Status() (UpdateStatus, error) {
	if s.ctl == nil {
		return UpdateStatus{}, notImplemented("Updates.Status")
	}
	st, err := s.ctl.UpdateStatus()
	if err != nil {
		return UpdateStatus{}, s.updateError(err, "Updates.Status")
	}
	return statusView(st), nil
}

// Check checks for an update now (06's CheckNow, at most 45 s) and returns the status it left: update.<code> for a
// check that failed (06 §8.10).
func (s *Updates) Check(ctx context.Context) (UpdateStatus, error) {
	if s.ctl == nil {
		return UpdateStatus{}, notImplemented("Updates.Check")
	}
	ctx, cancel := withTimeout(ctx, updateCheckTimeout)
	defer cancel()
	st, err := s.ctl.CheckUpdate(ctx)
	if err != nil {
		return UpdateStatus{}, s.updateError(err, "Updates.Check")
	}
	return statusView(st), nil
}

// Install applies the verified installer and quits (06's Apply, 03 §11.6, §16.2): busy while a share is live (03
// enforces it; the updater isn't told), update.not_ready without a verified installer, update.not_installed for a
// copy run from a folder, and update.<code> for a failed apply (hash_mismatch, install_failed, …).
func (s *Updates) Install(ctx context.Context) error {
	if s.ctl == nil {
		return notImplemented("Updates.Install")
	}
	if err := s.ctl.InstallUpdate(ctx); err != nil {
		return s.updateError(err, "Updates.Install")
	}
	return nil
}

// updateError maps the app's and 06's errors (03 §6.4): update.<code> for the updater's codes, update.not_ready,
// update.not_installed; the rest through the common table (busy, a deadline, a cancelled call).
func (s *Updates) updateError(err error, method string) error {
	switch {
	case errors.Is(err, app.ErrNotImplemented), errors.Is(err, update.ErrUnsupported):
		return notImplemented(method)
	case errors.Is(err, app.ErrUpdateNotReady):
		return newError(codes.CodeUpdateNotReady, nil)
	case errors.Is(err, update.ErrNotInstalled):
		return newError(codes.CodeUpdateNotInstalled, nil)
	}
	if code := update.CodeOf(err); code != "" {
		return newError(codes.Code(codes.UpdatePrefix+code), nil)
	}
	return mapError(err, s.log)
}
