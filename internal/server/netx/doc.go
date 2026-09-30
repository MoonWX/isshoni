// Package netx owns isshoni's network plumbing below HTTP (docs/m1/04-server-platform.md §7): the ICE transports
// the SFU runs on, public-IP detection and NAT classification, the address rewrite rules, the 443 first-byte
// multiplexer and the socket-level transfer counters.
//
// What exists so far (README slices S16 and S26, 04 slices S5 and S6):
//   - transport.go: Transport, NewTransport and Transport.Apply (04 §7.3): one UDP socket per usable local address
//     on listen.ice_udp in an ice.MultiUDPMuxDefault, the listen.ice_tcp listener and the 443 ICE sub-listener in
//     ice.TCPMuxDefaults combined in an ice.MultiTCPMuxDefault, the interface and IP filters, the rewrite rules and
//     the advertised addresses;
//   - udpconn.go and tcpconn.go: the byte-counting UDP socket (it keeps pion's allocation-free AddrPort path) and
//     the 7882/tcp listener with its per-IP ICE connection limit;
//   - publicip.go, stun.go and ifaces.go: DetectPublicAddrs (04 §7.4) on an injected STUN client and interface
//     lister, so every row of the classification table is tested offline;
//   - rewrite.go: the address plan of 04 §7.5 (rewrite rules, kept addresses, LAN flags);
//   - counter.go: TransferCounter (04 §11.3); cloud.go: the DMI cloud-provider table (04 §13.3);
//   - portmux.go and prefixconn.go: the 443 first-byte multiplexer (04 §7.2). PortMux routes TLS to TLS() for
//     http.Server and RFC 4571 framed ICE-TCP to ICE() for the Transport's 443 mux, answers plain HTTP with a hint,
//     closes the rest, and enforces the pending, per-IP and ICE limits keyed by IPKey (the ICE count is shared
//     with 7882/tcp).
//
// netx takes plain option structs and never imports config or the SFU (04 §2): the wiring fills TransportOptions
// and DetectOptions from the config, and 02's sfu calls Transport.Apply on each SettingEngine it builds.
package netx
