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
//     wrapper, and the after-commit steps of a revocation: cache first, then ConnCloser (§7.4–7.7);
//   - login.go (README S30): Login and Logout with the throttles of §7.3 in their order (auth-ip, auth-user-ip,
//     auth-user with the known-IP rule, auth-hash) and the audit rows of §10;
//   - setup.go (README S30): SetupAvailable, IssueSetupToken, CheckSetupToken and CompleteSetup (§7.8);
//   - alerts.go: admin alerts (§7.11).
//
// The other Service methods (invites and registration, self-service, admin users, password resets, the janitor)
// return a not-implemented error wrapping api internal until the later auth slices fill them in (03 §18 slices
// 8–13); the M2 device-flow methods come with their api DTOs in 03 slice 15.
//
// Service errors are *api.Error values with the codes of 03 §12.2: the rules' FieldErrors become validation_failed
// with a code per field, a refused throttle becomes rate_limited or server_busy with retryAfter, and an unexpected
// failure wraps internal together with its cause.
package auth
