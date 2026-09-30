// Package auth holds isshoni's account rules and credentials: usernames, passwords and their hashing, web sessions,
// setup, invite and reset tokens, throttles, roles, and the REST CSRF check (docs/m1/03-accounts-and-store.md §7).
//
// The package imports only the store and internal/protocol/api (04 §2). It never reads config: the wiring passes
// plain option structs.
//
// What exists so far (README slice S08: 03 slice 4 and the origin.go part of 03 slice 3):
//   - tokens.go: random tokens, keyed HMAC-SHA-256 token hashes, key fingerprints and the rotation check (§3.2,
//     §4.6);
//   - username.go: NormalizeUsername, the PRECIS username rules (§7.1);
//   - password.go: CheckPassword and the embedded common-password list (§7.2);
//   - argon2.go: argon2id hashing in PHC form, the concurrency semaphore and the dummy hash (§7.2);
//   - limiter.go: in-memory token buckets, the throttle table of §7.3 with the auth-hash budget, and IPKey;
//   - useragent.go: DescribeUserAgent (§7.4);
//   - origin.go: CSRFGuard, the REST cross-origin and Content-Type check (§7.5);
//   - errors.go: FieldError and the field codes of the rules, ErrNoCookie;
//   - service.go: the types of §7.13 that do not depend on the store (Origins, Method, the Reason* codes,
//     AdminAlert and AdminAlerter, ReqMeta, Link and the request inputs).
//
// The Service (New, Options, Principal, ConnCloser, sessions, setup, invites, registration, revocation and the rest
// of §7.13) needs the store and internal/protocol/api, which are built in parallel with this slice. It comes with the
// sessions and login slice (README S30): New validates Keys, purges rows after a key rotation (rotatedKeys), builds
// the hasher, the throttles and the CSRFGuard, and the service methods turn FieldError, errHashBusy and refused
// throttles into *api.Error values (validation_failed, server_busy, rate_limited).
package auth
