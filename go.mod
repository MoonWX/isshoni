module github.com/MoonWX/isshoni

go 1.26.0

toolchain go1.27.1

// Node's dependency trees are not Go code: keep them out of ./... patterns.
ignore (
	./docs/node_modules
	./web/node_modules
)

require (
	github.com/SherClockHolmes/webpush-go v1.4.0
	github.com/caddyserver/certmagic v0.25.6
	github.com/coder/websocket v1.8.15
	github.com/mholt/acmez/v3 v3.1.7
	github.com/pelletier/go-toml/v2 v2.4.3
	github.com/pion/dtls/v3 v3.1.10
	github.com/pion/ice/v4 v4.4.5
	github.com/pion/interceptor v0.1.49
	github.com/pion/logging v0.2.4
	github.com/pion/rtcp v1.2.19
	github.com/pion/rtp v1.10.5
	github.com/pion/sdp/v3 v3.0.20
	github.com/pion/stun/v4 v4.0.1
	github.com/pion/webrtc/v4 v4.2.22
	github.com/prometheus/client_golang v1.24.1
	github.com/rogpeppe/go-internal v1.16.0
	github.com/skip2/go-qrcode v0.0.0-20200617195104-da1b6568686e
	go.uber.org/goleak v1.3.0
	go.uber.org/zap v1.28.0
	golang.org/x/crypto v0.57.0
	golang.org/x/sys v0.48.0
	golang.org/x/text v0.42.0
	modernc.org/sqlite v1.60.1
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/caddyserver/zerossl v0.1.6 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/klauspost/cpuid/v2 v2.4.0 // indirect
	github.com/libdns/libdns v1.1.1 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/miekg/dns v1.1.73 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/pion/datachannel v1.6.3 // indirect
	github.com/pion/mdns/v2 v2.2.2 // indirect
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/sctp v1.11.3 // indirect
	github.com/pion/srtp/v3 v3.1.0 // indirect
	github.com/pion/transport/v5 v5.1.1 // indirect
	github.com/pion/turn/v5 v5.1.2 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.70.1 // indirect
	github.com/prometheus/procfs v0.21.1 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/wlynxg/anet v0.0.5 // indirect
	github.com/zeebo/blake3 v0.2.4 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.uber.org/zap/exp v0.3.0 // indirect
	golang.org/x/net v0.59.0 // indirect
	golang.org/x/time v0.14.0 // indirect
	golang.org/x/tools v0.50.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	modernc.org/libc v1.77.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)
