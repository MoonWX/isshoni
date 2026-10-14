package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/ops"
	"github.com/MoonWX/isshoni/internal/server/servertest"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
)

// The admin's user endpoints and the reset link on a running server (README S64): the REST handlers of this
// package, the real auth.Service, the wiring's ConnCloser and Signal adapters and 01's hub, in one process
// (servertest). The service and handler tests say what each call asks of the hub; this one says that the browsers
// a user would have open see it.

// joinDefaultRoom makes the socket join the Lounge, as the web app does on its room page.
func joinDefaultRoom(t *testing.T, c *signaltest.Client) {
	t.Helper()
	id := c.NextID()
	if err := c.Send(protocol.MessageTypeRoomJoin, id, protocol.RoomJoin{RoomID: "lounge"}); err != nil {
		t.Fatal(err)
	}
	if reply := nextOf(t, c, protocol.MessageTypeOK, protocol.MessageTypeError); reply.Type != protocol.MessageTypeOK || reply.Re != id {
		t.Fatalf("room.join answered %s %s", reply.Type, reply.Data)
	}
}

// awaitParticipant waits for a room.state on the socket in which the participant with that user ID is as ok wants.
func awaitParticipant(t *testing.T, what string, c *signaltest.Client, userID string, ok func(p protocol.ParticipantInfo) bool) {
	t.Helper()
	for {
		// nextOf fails the test when no such message comes within its timeout.
		state, err := protocol.Decode[protocol.RoomState](nextOf(t, c, protocol.MessageTypeRoomState))
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		for _, p := range state.Participants {
			if p.UserID == userID && ok(p) {
				return
			}
		}
	}
}

// usersOf is the admin's user list as the browser gets it.
func usersOf(t *testing.T, b *browser) map[string]api.AdminUser {
	t.Helper()
	var list api.AdminUsersResponse
	if err := json.Unmarshal([]byte(b.must(http.StatusOK, http.MethodGet, "/api/v1/admin/users", nil)), &list); err != nil {
		t.Fatal(err)
	}
	out := map[string]api.AdminUser{}
	for _, u := range list.Users {
		out[u.Username] = u
	}
	return out
}

// loginRefused sends a login from a new browser and returns the error code of its refusal.
func loginRefused(t *testing.T, srv *servertest.Server, username, password string) string {
	t.Helper()
	status, body := newBrowser(t, srv).call(http.MethodPost, "/api/v1/auth/login", api.LoginRequest{Username: username, Password: password})
	var env api.ErrorResponse
	if err := json.Unmarshal([]byte(body), &env); err != nil || status < 400 {
		t.Fatalf("login as %s = %d %s, want a refusal", username, status, body)
	}
	return env.Error.Code
}

// TestAdminUsersOnAServer: the admin's Users page and a reset link against a real server. The list knows who is
// online; a rename and a role change reach the room without closing a socket; a reset link, signing out, disabling
// and deleting close the sockets their rows of 03 §7.7 name, with the code 01 gives each.
func TestAdminUsersOnAServer(t *testing.T) {
	srv, admin := wiredServer(t)
	sam := friend(t, srv, admin, "Sam")
	second := login(t, srv, "Sam", wiredPassword)
	offline := friend(t, srv, admin, "Robin")
	adminWS, samWS, secondWS := admin.socket(), sam.socket(), second.socket()
	joinDefaultRoom(t, adminWS)
	joinDefaultRoom(t, samWS)

	// The list: every account, and who has a socket open right now.
	users := usersOf(t, admin)
	if len(users) != 3 || !users[wiredAdmin].Online || !users["Sam"].Online || users["Robin"].Online ||
		users["Sam"].Sessions != 2 || users["Sam"].InvitedBy == nil || users["Sam"].InvitedBy.Username != wiredAdmin {
		t.Fatalf("the user list = %+v", users)
	}
	samID := users["Sam"].ID
	user := "/api/v1/admin/users/" + samID
	if !offline.signedIn() {
		t.Fatal("the third account should be signed in")
	}

	// A rename and a role change: the room sees the new name and the badge, and nothing closes.
	name := "Samuel"
	admin.must(http.StatusOK, http.MethodPatch, user, api.PatchUserRequest{Username: &name})
	awaitParticipant(t, "the room after the rename", adminWS, samID, func(p protocol.ParticipantInfo) bool { return p.Name == name })
	role := api.RoleAdmin
	if status, body := admin.call(http.MethodPatch, user, api.PatchUserRequest{Role: &role}); status != http.StatusForbidden ||
		!strings.Contains(body, api.CodeWrongPassword) {
		t.Fatalf("a grant without the admin's password = %d %s, want 403 wrong_password", status, body)
	}
	admin.must(http.StatusOK, http.MethodPatch, user, api.PatchUserRequest{Role: &role, CurrentPassword: wiredPassword})
	awaitParticipant(t, "the room after the grant", adminWS, samID, func(p protocol.ParticipantInfo) bool { return p.Admin })
	wantOpen(t, "the renamed user's browser", samWS)
	wantOpen(t, "the renamed user's other browser", secondWS)
	// The new admin's next request is an admin's; after the demotion it is none again.
	sam.must(http.StatusOK, http.MethodGet, "/api/v1/admin/users", nil)
	role = api.RoleUser
	admin.must(http.StatusOK, http.MethodPatch, user, api.PatchUserRequest{Role: &role})
	sam.must(http.StatusForbidden, http.MethodGet, "/api/v1/admin/users", nil)
	awaitParticipant(t, "the room after the demotion", adminWS, samID, func(p protocol.ParticipantInfo) bool { return !p.Admin })

	// A reset link: every browser of the user is signed out, and the link logs a new one in.
	var link api.ResetLink
	if err := json.Unmarshal([]byte(admin.must(http.StatusCreated, http.MethodPost, user+"/password-reset", struct{}{})), &link); err != nil {
		t.Fatal(err)
	}
	token, ok := strings.CutPrefix(link.URL, srv.URL+"/reset#")
	if !ok {
		t.Fatalf("reset link %q, want %s/reset#<token>", link.URL, srv.URL)
	}
	wantRevoked(t, "the user's browser after a reset link was issued", samWS)
	wantRevoked(t, "the user's other browser after a reset link was issued", secondWS)
	wantOpen(t, "the admin's browser", adminWS)
	if sam.signedIn() || second.signedIn() {
		t.Error("the user's browsers are still signed in after the reset link was issued")
	}
	if code := loginRefused(t, srv, name, wiredPassword); code != api.CodeInvalidCredentials {
		t.Errorf("the old password after the reset link was issued: %s, want invalid_credentials", code)
	}
	if !usersOf(t, admin)[name].ResetPending {
		t.Error("the user list does not say that a reset is pending")
	}
	const changed = "another long passphrase"
	fresh := newBrowser(t, srv)
	if got := fresh.must(http.StatusOK, http.MethodPost, "/api/v1/auth/reset/check", api.TokenRequest{Token: token}); got != `{"username":"Samuel"}`+"\n" {
		t.Errorf("reset/check answered %s", got)
	}
	fresh.must(http.StatusOK, http.MethodPost, "/api/v1/auth/reset/complete", api.ResetCompleteRequest{Token: token, Password: changed})
	if !fresh.signedIn() {
		t.Fatal("the reset link did not log the browser in")
	}
	fresh.must(http.StatusNotFound, http.MethodPost, "/api/v1/auth/reset/check", api.TokenRequest{Token: token})
	freshWS := fresh.socket()

	// Sign out everywhere: the user's sockets close, the password stays.
	if got := admin.must(http.StatusOK, http.MethodPost, user+"/sign-out", struct{}{}); got != `{"sessions":1,"devices":0}`+"\n" {
		t.Errorf("sign-out answered %s", got)
	}
	wantRevoked(t, "the signed-out user's browser", freshWS)

	// Disable: the socket ends with account_disabled (close 4403), and the password is refused with the same code.
	third := login(t, srv, name, changed)
	thirdWS := third.socket()
	status := api.UserStatusDisabled
	admin.must(http.StatusOK, http.MethodPatch, user, api.PatchUserRequest{Status: &status})
	wantEnded(t, "the disabled user's browser", thirdWS, protocol.ErrorCodeAccountDisabled, protocol.CloseCodeForbidden)
	if code := loginRefused(t, srv, name, changed); code != api.CodeAccountDisabled || third.signedIn() {
		t.Errorf("a login of the disabled account: %s, want account_disabled", code)
	}

	// Enable, then delete: the sockets close and the account is gone.
	status = api.UserStatusActive
	admin.must(http.StatusOK, http.MethodPatch, user, api.PatchUserRequest{Status: &status})
	fourth := login(t, srv, name, changed)
	fourthWS := fourth.socket()
	admin.must(http.StatusNoContent, http.MethodDelete, user, nil)
	wantRevoked(t, "the deleted user's browser", fourthWS)
	wantOpen(t, "the admin's browser at the end", adminWS)
	if users := usersOf(t, admin); len(users) != 2 || fourth.signedIn() || !offline.signedIn() {
		t.Errorf("after the deletion: users %+v", users)
	}
}

// TestOperatorCLIOnAServer: the admin socket's account commands (03 §12.6) reach the same calls as store.CLIActor.
// `reset-password` is how a sole admin who is locked out gets back in: no password is asked for, and the own
// account is no obstacle (03 §7.10). `set-role` follows the last-admin rule; `disable` closes the user's sockets.
func TestOperatorCLIOnAServer(t *testing.T) {
	srv, admin := wiredServer(t)
	sam := friend(t, srv, admin, "Sam")
	adminWS, samWS := admin.socket(), sam.socket()
	ctx, cancel := context.WithTimeout(context.Background(), wiredTimeout)
	defer cancel()
	cli := ops.DialAdmin(srv.AdminSocket)

	// reset-password for the only admin, by any spelling of the name.
	link, err := cli.ResetLink(ctx, strings.ToUpper(wiredAdmin))
	token, ok := strings.CutPrefix(link.URL, srv.URL+"/reset#")
	if err != nil || !ok {
		t.Fatalf("reset-password = %+v, %v; want %s/reset#<token>", link, err, srv.URL)
	}
	wantRevoked(t, "the admin's browser after the CLI's reset", adminWS)
	wantOpen(t, "another user's browser", samWS)
	if code := loginRefused(t, srv, wiredAdmin, wiredPassword); code != api.CodeInvalidCredentials {
		t.Errorf("the admin's old password: %s, want invalid_credentials", code)
	}
	const changed = "another long passphrase"
	admin.must(http.StatusOK, http.MethodPost, "/api/v1/auth/reset/complete", api.ResetCompleteRequest{Token: token, Password: changed})
	if users := usersOf(t, admin); len(users) != 2 || users[wiredAdmin].ResetPending {
		t.Fatalf("the admin's user list after completing the link = %+v", users)
	}
	if _, err := cli.ResetLink(ctx, "nobody"); !api.IsCode(err, api.CodeUserNotFound) {
		t.Errorf("reset-password for an unknown name: %v, want user_not_found", err)
	}

	// set-role: no password; the last active admin stays one.
	if err := cli.SetRole(ctx, wiredAdmin, api.RoleUser); !api.IsCode(err, api.CodeLastAdmin) {
		t.Errorf("set-role user for the only admin: %v, want last_admin", err)
	}
	if err := cli.SetRole(ctx, "sam", api.RoleAdmin); err != nil {
		t.Fatalf("set-role admin: %v", err)
	}
	sam.must(http.StatusOK, http.MethodGet, "/api/v1/admin/users", nil) // an admin on the next request
	if err := cli.SetRole(ctx, wiredAdmin, api.RoleUser); err != nil {
		t.Fatalf("set-role user with another admin around: %v", err)
	}
	admin.must(http.StatusForbidden, http.MethodGet, "/api/v1/admin/users", nil)
	wantOpen(t, "the promoted user's browser", samWS)

	// disable and enable.
	if err := cli.SetDisabled(ctx, "Sam", true); !api.IsCode(err, api.CodeLastAdmin) {
		t.Errorf("disable for the only admin: %v, want last_admin", err)
	}
	adminWS = admin.socket()
	if err := cli.SetDisabled(ctx, wiredAdmin, true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	wantEnded(t, "the disabled user's browser", adminWS, protocol.ErrorCodeAccountDisabled, protocol.CloseCodeForbidden)
	if code := loginRefused(t, srv, wiredAdmin, changed); code != api.CodeAccountDisabled {
		t.Errorf("a login of the disabled account: %s, want account_disabled", code)
	}
	if err := cli.SetDisabled(ctx, wiredAdmin, false); err != nil {
		t.Fatalf("enable: %v", err)
	}
	login(t, srv, wiredAdmin, changed)
	wantOpen(t, "the other user's browser at the end", samWS)
}
