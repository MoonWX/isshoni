// Package push is isshoni's Web Push sender (docs/m1/04-server-platform.md §14): it turns "a share went live" and
// admin alerts into encrypted notifications (RFC 8291 aes128gcm, RFC 8292 VAPID) for the browsers that subscribed.
// 03 owns the push REST endpoints and their tables; this package is what those handlers call, and what the hub and
// auth trigger through the wiring (04 §6.6).
//
// Files:
//   - service.go: Options, Service, New, Run, the triggers (ShareStarted, AdminAlert, SendTest), the queue, its
//     workers, and the result table of 04 §14.5 (record, delete, retry);
//   - store.go: Store, RecipientFilter and Subscription, the small interface over 03's tables (04 §14.7);
//   - payload.go: the three payloads of 04 §14.3 (api.PushPayload: data, never English text);
//   - limits.go: the dedup window per (room, sharer), the token bucket per recipient and the Topic header
//     (04 §14.4);
//   - sender.go: Sender, and the real one on webpush-go with the guarded HTTP client;
//   - ssrf.go: the SSRF guard (04 §14.5): ValidateEndpoint's URL and DNS rules at subscribe time, and the dial
//     hook that checks the address actually being connected to at send time;
//   - prune.go: the VAPID fingerprint check that New runs (a rotated key deletes every subscription, 04 §5.2) and
//     the daily prune (04 §14.6).
//
// The subscription endpoint comes from a browser, so it is untrusted input and a capability URL: it travels as
// logx.Secret, and logs carry only its host (push_host).
//
// push imports config, logx, netx and internal/protocol/api, never store, auth, signal or httpapi (04 §2): the
// wiring implements Store over 03's store, converts 01's signal.PushShareStarted and 03's auth.AdminAlert, and
// wraps the Service as httpapi.Push. With push.enabled = false the wiring builds no Service at all.
package push
