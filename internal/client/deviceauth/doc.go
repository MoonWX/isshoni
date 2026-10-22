// Package deviceauth links a native app to isshoni servers and keeps it signed in (docs/m2/02-device-auth.md §11):
//
//   - Client is the REST side of one server: /info, the RFC 8628 device flow (/device/code, /device/token), the
//     password fallback (/device/password), the refresh grant and /device/revoke. It follows no redirect, reads at
//     most 64 KiB, and checks the verification URIs a server hands out before anyone opens them.
//   - Manager is one server's link: the state machine of 02 §11.3 (unlinked, linking, linked, offline,
//     incompatible), the credential in a credstore.Store, the 15-minute access token and its single-flight refresh
//     with rotation, the exchange id and the retry series of §11.4, the compat checks of §13 and Unlink. It is
//     01-client-core's core.Auth source: AccessToken(ctx, forceRefresh), with errors whose chains answer
//     RelinkRequired() (ErrNotLinked) and UpdateRequired() (ErrIncompatible); every other error is transient.
//   - Servers is the app's list: servers.json in the data root (at most 20 servers, one active), one Manager per
//     server, Add adopting the server's own public origin, and ForgetAll for the uninstaller.
//
// The refresh token never leaves Go: the desktop SPA gets access tokens through 03-desktop-app's binding
// (AccessTokenWithExpiry). No log line or error of this package holds a token, a device or user code, an exchange
// id, a password, a user name or a device name; State.LogValue logs the status only.
//
// The package stays gomobile-friendly (no cgo, no OS API calls): it imports the standard library,
// internal/protocol, internal/protocol/api, internal/client/credstore, internal/client/compat and
// internal/client/deeplink (02 §11.1).
package deviceauth
