// Package auth holds isshoni's account rules and credentials: usernames, passwords and their hashing, web sessions,
// setup, invite and reset tokens, throttles, roles, and the REST CSRF check (docs/m1/03-accounts-and-store.md §7).
//
// The package imports only the store and internal/protocol/api (04 §2). It never reads config: the wiring passes
// plain option structs.
//
// The rules and primitives (README slice S08: 03 slice 4 and the origin.go part of 03 slice 3):
//   - tokens.go: random tokens, keyed HMAC-SHA-256 token hashes, key fingerprints and the startup check that purges
//     the rows a rotated key protected (§3.2, §4.6);
//   - username.go: NormalizeUsername, the PRECIS username rules (§7.1);
//   - password.go: CheckPassword and the embedded common-password list (§7.2);
//   - argon2.go: argon2id hashing in PHC form, the concurrency semaphore and the dummy hash (§7.2);
//   - limiter.go: in-memory token buckets, the throttle table of §7.3 with the auth-hash budget, and IPKey;
//   - useragent.go: DescribeUserAgent (§7.4);
//   - origin.go: CSRFGuard, the REST cross-origin and Content-Type check (§7.5);
//   - errors.go: FieldError and the field codes of the rules, ErrNoCookie.
//
// The Service (§7.13), built by New:
//   - service.go: the whole Go API of §7.13 (README S24): Service with every M1 method, Options, New, Principal,
//     ActorOf, ConnSelector/ConnCloser, the results and inputs. New validates the keys and origins, runs the key
//     fingerprint check and builds the hasher, the throttles, the CSRF guard and the session cache;
//   - session.go (README S30): the session cookie, Authenticate (REST) and AuthenticateCookie (/ws), the 30 s session
//     cache, Touch (the hub's Revalidate), MaybeRotate (the daily token rotation with its 60 s grace), the CSRF
//     wrapper, and the after-commit steps of revoking single sessions: cache first, then ConnCloser (§7.4–7.7);
//   - login.go (README S30): Login and Logout with the throttles of §7.3 in their order (auth-ip, auth-user-ip,
//     auth-user with the known-IP rule, auth-hash) and the audit rows of §10. A login takes its tokens of the two
//     per-username buckets before the hash and gets them back unless the password check failed, so parallel
//     guesses cannot outrun the limits;
//   - setup.go (README S30): SetupAvailable, IssueSetupToken, CheckSetupToken and CompleteSetup (§7.8);
//   - invite.go (README S42): CreateInvite, RevokeInvite and CheckInvite (§7.9): who may invite (admins and the CLI,
//     members while membersCanInvite is on, nobody in the closed mode), the limits (100 active invites per server,
//     10 per member) and why a link doesn't work;
//   - register.go (README S42): Register in the three registration modes (§7.9), with its checks in their order:
//     auth-ip and register-ip, the mode, the invite, the fields, the name, the auth-hash budget, and one Write that
//     checks again. An invite gives an active account that is logged in at once; a sign-up without one (approval
//     mode) gives a pending account and the signup_pending admin alert, at most one per 10 minutes. And the
//     approval queue: Approve, Reject and RejectAll;
//   - settings.go (README S42): UpdateSettings, the settings patch of §9 with the registration_mode_changed admin
//     alert. It is an addition to §7.13;
//   - revoke.go (README S58): the revocation table of §7.7. One rule per row that takes a user's credentials as a
//     whole (which sessions, devices and reset link its Write deletes, and the reason its connections are closed
//     with), for the user's own actions and for the admin's, and the steps that follow every commit: the session
//     cache first, then ConnCloser. Also loadSelf, the caller's check that every self-service Write starts with;
//   - selfservice.go (README S58): RevokeSession, RevokeOtherSessions, LogoutEverywhere, ChangePassword, DeleteSelf
//     and RevokeDevice, each a row of that table with its audit row. Changing the password and deleting the account
//     ask for the password again (verifyOwnPassword), which counts in the two per-username buckets of failed
//     password checks like a login; the only active admin can't delete the account (§7.11). A password change gives
//     the caller's session a new token as a rotation of §7.4: the token the request arrived with works for 60 s
//     more;
//   - admin.go (README S64): UpdateUser (rename, role, status), DeleteUser, SignOutUser and UserByUsername (§7.11).
//     At least one active admin always exists (last_admin); an admin can't disable or delete the own account
//     through them (self_action_forbidden); making someone an admin asks for the acting admin's own password
//     (wrong_password), which the CLI is exempt from. A rename, a role change and a status change invalidate the
//     session cache; disabling, deleting and signing out are rows of the revocation table; a role change raises
//     the admin_granted or admin_revoked alert;
//   - reset.go (README S64): the admin-issued password reset of §7.10. IssuePasswordReset clears the password,
//     takes every session and device of the user away and returns a one-time /reset#token link (24 h); a reset of
//     another admin asks for the acting admin's password and raises the admin_password_reset alert, and nobody
//     resets the own account this way. CheckPasswordReset and CompletePasswordReset are the public side: the
//     token is checked before the password rules and the hash, and the completion sets the password and opens a
//     session in one Write;
//   - alerts.go: admin alerts (§7.11);
//   - janitor.go (README S70): RunJanitor, below.
//
// The device flow (docs/m2/02-device-auth.md §5 and §14; M2 README WA30):
//   - deviceflow.go: StartDeviceFlow (POST /device/code: auth-ip, device-code-ip and, for IPv6, device-code-ip48,
//     the app's fields, then the server-wide cap of 5,000 pending codes in the Write that adds the code),
//     LookupUserCode and DecideUserCode (the approval page, cookie sessions only, with the device-lookup bucket that
//     keeps its token only for a wrong code), and the device_code grant of DeviceToken: one Write per poll that
//     decides the RFC 8628 answer (authorization_pending, slow_down with the interval raised by 5 s up to 60 s,
//     access_denied, expired_token, invalid_grant) and commits its bookkeeping. An approved code's poll runs the
//     success steps of a link (linkDevice): the cap of 20 devices with device.revoked {via: "cap"} rows, the device,
//     its first token pair and device.linked; device.approved and device.denied are the approver's rows;
//   - devicepassword.go: PasswordDeviceLogin, the in-app password fallback, with Login's order and buckets
//     (checkLogin in login.go) and the same success steps with linked_via password;
//   - devicename.go: NormalizeDeviceName (PRECIS Nickname, 1–64 code points);
//   - limiter.go: the device buckets and IPKey48.
//
// Device tokens (docs/m2/02-device-auth.md §6; M2 README WA39):
//   - refresh.go: the refresh_token grant of DeviceToken. Refresh tokens rotate on every use; a spent token that
//     comes back revokes its device (device.refresh_reused, the refresh_token_reused alert, RefreshReusedError for
//     httpapi's notification), except the client's retry of one refresh (the same exchangeId, any time) and a restart
//     within 120 s of the spend (once), and in both cases only while the pair it was exchanged for is unused. A grace
//     re-issue keeps the child it replaces as a spent row, so that a second holder who presents it trips reuse
//     detection;
//   - bearer.go: AuthenticateBearerToken and Authenticate's bearer path (the header's grammar and the token's shape
//     checked before any hashing or DB access, then a 30 s cache that never outlives the token), the bearer
//     principal with DeviceName, the device half of TouchPrincipal (Touch keeps M1's signature for the wiring) with
//     its 5-minute last-seen write, and the after-commit steps of a device's revocation: the bearer cache forgets the
//     device, or the user (forgetUser), before ConnCloser runs;
//   - selfservice.go: RenameDevice (device.renamed {from, to}), RevokeOwnDevice (the app's sign-out) and the bearer
//     cases of RevokeSession, RevokeDevice and LogoutEverywhere, with loadCaller (revoke.go) checking a device's
//     principal inside the Write. The calls only a web session makes (Logout, ChangePassword, DeleteSelf,
//     RevokeOtherSessions) refuse a bearer with forbidden {credential: session}.
//
// A call that acts for an account takes a store.Actor and reads that account inside its own transaction (actingAs
// in service.go): the CLI acts as an admin, a user actor has the role its row has at that moment, and an account
// that is gone or not active is forbidden. A self-service call takes the request's Principal and reads the account
// and the caller's session the same way (loadSelf), or for a device's principal its device (loadCaller): without a
// live session or device it is unauthenticated.
//
// The janitor (README S70; janitor.go): RunJanitor prunes the expired rows of §4.7 through store.DB.Prune at startup
// and then every hour, until its context ends.
//
// Service errors are *api.Error values with the codes of 03 §12.2: the rules' FieldErrors become validation_failed
// with a code per field, a refused throttle becomes rate_limited or server_busy with retryAfter, and an unexpected
// failure wraps internal together with its cause.
package auth
