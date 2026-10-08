// Package tlsmgr gets the server its certificate and keeps it current (docs/m1/04-server-platform.md §8). One
// Manager serves one TLS mode:
//
//	auto    a Let's Encrypt certificate for the domain (ACME through certmagic; http-01 and tls-alpn-01)
//	ip      a Let's Encrypt certificate for the public IP address, from the "shortlived" profile (RFC 8738)
//	manual  the operator's own files, read again when they change and on Reload
//	off     no certificate: a reverse proxy terminates TLS
//
// The manager opens no ports. The wiring (internal/server) binds 443 and 80 and hands the manager's parts to its
// own servers: TLSConfig goes to the main http.Server behind netx.PortMux, and HTTPHandler is the handler of the
// port 80 server (ACME http-01, then the redirect to HTTPS; the app itself in off mode). certmagic answers
// challenges through those two only: its ACME issuer is told the bound addresses, finds them taken, and leaves
// them alone.
//
// Files:
//   - manager.go: Options, Status, Manager and its life (New, Start, Shutdown), the certmagic setup of 04 §8.2
//     and the readiness check "tls";
//   - storage.go: certmagic's file storage with a count of what goes on in it, so that Shutdown returns only when
//     nothing writes below the storage directory any more;
//   - manual.go: manual mode (04 §8.4): loading and checking the pair, the 60 s poll for changed files;
//   - redirect.go: the port 80 handler (04 §8.3);
//   - hints.go: the stable codes of Status.LastErrorCode with their English fix texts (04 §8.7).
//
// What the certificate needs from outside shows in Status and in the log, never as a failed start: a server
// without a certificate runs and is not ready (04 §6.2), so that doctor and the admin socket can say why.
//
// There is no self-signed mode (04 §8.6): every mode ends with a publicly trusted certificate, or with a proxy
// that has one. Tests bring their own CA: servertest.Options.TLS runs manual mode with a private one, and the ACME
// tests run against Pebble (the acme job of the CI workflow; see acme_test.go for running them locally).
//
// Imports (04 §2): config, logx, version, netx and internal/protocol(/api); never httpapi, store, auth, signal, sfu
// or sfuplane.
package tlsmgr
