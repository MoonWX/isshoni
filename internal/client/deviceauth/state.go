package deviceauth

import (
	"log/slog"
	"time"

	"github.com/MoonWX/isshoni/internal/client/compat"
	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// Status is where one server's link stands (02 §11.3).
type Status string

const (
	StatusUnlinked     Status = "unlinked"     // no credential: show "Link this app"
	StatusLinking      Status = "linking"      // a code is shown; polling
	StatusLinked       Status = "linked"       // a credential, and the server answers
	StatusOffline      Status = "offline"      // linked, but the server can't be reached now
	StatusIncompatible Status = "incompatible" // compat says update_app or update_server
)

// Notice says why the state changed when the user didn't do it. Shown once, then cleared by the UI.
type Notice string

const (
	NoticeNone            Notice = ""
	NoticeDenied          Notice = "denied"           // access_denied
	NoticeCodeExpired     Notice = "code_expired"     // expired_token / invalid_grant while linking
	NoticeSignedOut       Notice = "signed_out"       // invalid_grant at a refresh; device revoked or expired
	NoticeRefreshReused   Notice = "refresh_reused"   // invalid_grant {reason: reused}
	NoticeAccountDisabled Notice = "account_disabled" // SessionEnded(account_disabled), then invalid_grant
	NoticeSaveFailed      Notice = "save_failed"      // credstore write failed; linked for this run only
)

// LinkState is the code a linking app shows. Its URIs are Go's own canonical strings (02 §11.2), never the server's,
// so they are safe to open, draw as a QR code and copy.
type LinkState struct {
	UserCode                string    // "WDJB-MJHT" (FormatUserCode of the checked code)
	VerificationURI         string    // origin + "/link", built by Go after §11.2's check
	VerificationURIComplete string    // origin + "/link?code=" + UserCode; also the QR code's content
	ExpiresAt               time.Time // when the code expires
}

// LogValue logs only when the code expires: user codes are never logged (README §4 Logging).
func (l LinkState) LogValue() slog.Value {
	return slog.GroupValue(slog.Time("expires_at", l.ExpiresAt))
}

// State is one server's link as the UI shows it. It holds no token, code secret or exchange id.
type State struct {
	Origin     string
	ServerName string         // from /info
	Status     Status         // see the constants
	Link       *LinkState     // StatusLinking
	User       *api.User      // linked, offline
	Device     *api.DeviceRef // linked, offline
	Notice     Notice         // why the state changed when the user didn't do it
	Compat     compat.Check   // StatusIncompatible (and the last check otherwise)
}

// LogValue logs the origin, the status, the notice and the compat verdict. The user code, the user name and the
// device name stay out (README §4 Logging: none of them is ever logged).
func (s State) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("origin", s.Origin),
		slog.String("status", string(s.Status)),
		slog.String("notice", string(s.Notice)),
		slog.String("verdict", string(s.Compat.Verdict)),
		slog.String("reason", string(s.Compat.Reason)),
	)
}

// clone returns s with its own copies of the pointed-to values, so an OnState callback can keep or change it.
func (s State) clone() State {
	if s.Link != nil {
		l := *s.Link
		s.Link = &l
	}
	if s.User != nil {
		u := *s.User
		s.User = &u
	}
	if s.Device != nil {
		d := *s.Device
		s.Device = &d
	}
	return s
}

// sameState reports whether a and b show the same thing.
func sameState(a, b State) bool {
	if a.Origin != b.Origin || a.ServerName != b.ServerName || a.Status != b.Status || a.Notice != b.Notice ||
		a.Compat != b.Compat {
		return false
	}
	if (a.Link == nil) != (b.Link == nil) || a.Link != nil && (a.Link.UserCode != b.Link.UserCode ||
		a.Link.VerificationURI != b.Link.VerificationURI ||
		a.Link.VerificationURIComplete != b.Link.VerificationURIComplete || !a.Link.ExpiresAt.Equal(b.Link.ExpiresAt)) {
		return false
	}
	return sameUser(a.User, b.User) && sameDevice(a.Device, b.Device)
}

func sameUser(a, b *api.User) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.ID == b.ID && a.Username == b.Username && a.Role == b.Role && a.CreatedAt.Equal(b.CreatedAt)
}

func sameDevice(a, b *api.DeviceRef) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
