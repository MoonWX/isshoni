# Spike S4: Pion SFU (simulcast H.264 + keyframe-gated layer switching)

Throwaway spike for plan milestone M0. **Question:** can a small custom Pion SFU inside the isshoni binary receive
simulcast H.264 + Opus screen shares and forward a per-viewer layer, switching cleanly? The alternative is to adopt
the LiveKit sidecar (Plan B).

Not production code. M1 reimplements this properly under `internal/server/sfu`, with the design changes listed at the
bottom.

## Run

```bash
go run . -addr localhost:8080 -loopback   # -loopback only for same-machine tests
```

Open http://localhost:8080 in two browsers:
- **Sharer**: pick an H.264 profile and use "Share screen/window…" or "Share test pattern" (canvas + tone, no picker).
- **Viewer**: click a tile's High/Low/Off, or "Auto-switch test", which toggles layers and measures switch time and
  freezes. "Copy results JSON" collects everything.

Automated checks:

```bash
go test -race ./...                          # munger unit tests + in-process Pion integration (incl. 10×10)
cd e2e && npm install && npm run e2e         # headless Google Chrome: sharer → SFU → viewer, per H.264 profile
node e2e/firefox.mjs                         # Chrome ↔ installed Firefox (see results: blocked on this Mac)
```

## What it does

- Two `webrtc.API`s (publish / subscribe) share **one UDP port** (ICE UDP mux, 7882).
- The publish side negotiates TWCC and simulcast header extensions. The subscribe side has no transport-cc.
- `DownTrack` (a custom `TrackLocal`) runs one queue and goroutine per viewer.
  `munger` rewrites seq/timestamp so each viewer sees one continuous stream. It switches layers only on an SPS
  (STAP-A or single NAL). Publisher header extensions are stripped.
- Keyframe requests (PLI) to the publisher are throttled to 1 per 500 ms per layer. They are sent on layer switch, on
  viewer PLI/FIR, while waiting for a keyframe, and when the viewer's DTLS comes up.
- Signaling: WebSocket JSON with two PeerConnections per client (pub: the client offers; sub: the server offers).
  Trickle ICE is buffered until the remote description exists.

## Results (2026-09-28, MacBook Pro M5, Go 1.26.5, pion/webrtc v4.2.22)

| Criterion (plan S4) | Result |
|---|---|
| Layer switch within one keyframe interval | ✅ Go clients: high→low in **42–67 ms** (PLI → next keyframe). Chrome→Chrome: **median 52 ms, max 106 ms**, 10/10 switches detected, all four profiles |
| Zero decode errors on switches | ✅ Chrome viewer: 0 freezes, 0 PLIs from the viewer, ≤2 in-flight "undecoded" frames (noise) over 10 switches per profile |
| Continuity | ✅ Consecutive seq and monotonic timestamps across every switch/pause; every stream and switch starts on SPS (asserted for every subscription) |
| 10 publishers × 10 subscribers in-process | ✅ All 100 video subscriptions got the right layer in **1.85 s**; audio only on focused shares; integrity checks pass; stable over 3 runs with `-race` |
| Chrome decodes H.264 profiles from Chrome sharer | ✅ Baseline, Constrained Baseline, Main, High: 1280×720@60 plus 320×180@15, hardware decode (VideoToolbox), 8–9 ms jitter buffer |
| Safari 27 (macOS), manual by owner | ✅ Publishes simulcast **Constrained High 640c1f** (720p59 + 180p15) and decodes it: 0 freezes, 12 ms jitter buffer |
| Chrome 154 (macOS), manual by owner | ✅ High 64001f simulcast, 2,810 frames decoded, 0 freezes |
| Firefox 156 viewer (Windows Server 2025 VM, headless Edge 153 sharing CB 42e01f) | ✅ Decodes through the SFU: 6/6 switches detected (median 399 ms on a 2-vCPU VM with software encoding), 0 packets lost, 1 freeze. ⚠ In a **fresh Firefox profile the first join fails**: Firefox negotiates H.264 only after it has downloaded the OpenH264 plugin (in the background, within about a minute), and Windows Server has no Media Foundation. The server currently rejects the subscription ("codec is not supported by remote"). M1 must detect this and show "your browser is still getting its video decoder, retrying…", then renegotiate. It didn't connect on the owner's Mac (Local Network privacy / TUN proxy). iOS Safari viewer still to test |

**Gate decision:** the custom Pion SFU works for Chrome and Safari, so keep it; no LiveKit sidecar needed. Firefox is
re-tested on Windows.

Two more findings for M1:
- Browsers send two variants of High: Safari 640c1f, Chrome 6400xx. WebRTC encoders never use B-frames, so treat them
  as compatible when matching.
- The same caveat means any codec match between High and Constrained High must be tested against real decoders.

### Findings that change the M1 design

1. **Pion silently drops RTP written before the subscriber's DTLS is up** (`srtpWriterFuture.WriteRTP` returns
   `0, nil`). The first keyframe was lost and viewers started mid-GOP (the 10×10 test caught it). Fix: a DownTrack
   forwards only once the subscribe PC is `connected`, then requests a fresh keyframe.
2. **Simulcast receivers need `ReadSimulcastRTCP(rid)`.** Plain `ReadRTCP` errors when several rids share a receiver.
3. **Firefox 156 decodes only H.264 Constrained Baseline and Baseline** (receiver capabilities 42e01f, 42001f; no Main
   or High). This confirms the plan's codec policy: native senders use High only when every viewer supports it,
   otherwise Constrained Baseline.
4. **Chrome on macOS needs the High profile to hardware-encode both simulcast layers.**
   `SimulcastEncoderAdapter (VideoToolbox, VideoToolbox)` for 64001f. Constrained Baseline 42e01f uses OpenH264
   software on both layers; Baseline and Main use software for the small layer. Web sharers should prefer High when
   the room allows it.
5. **Chrome advertises High as `640034`, not `640c1f`.** Profile matching must compare profile_idc and constraint bits
   and ignore the level (as the spike's `h264FmtpKey` does).
6. **macOS Local Network privacy blocks browsers that lack the permission from LAN ICE candidates.** Automated
   Firefox failed every pair to the Mac's own LAN/IPv6 addresses while Chrome succeeded. This matters for LAN
   self-hosting and same-machine testing: `doctor` and the connection test should detect it and explain the
   permission. A VPS with a public IP is not affected.
7. Headless Chrome can't auto-accept `getDisplayMedia`, so automated tests use a canvas + WebAudio source. That still
   covers encode → SFU → decode → switching; the capture API itself stays a manual check.

## Manual check still needed (Firefox + Safari viewers, about 2 minutes)

1. `go run . -addr localhost:8080` (in this folder).
2. In **Chrome**: open http://localhost:8080 → **Join** → choose the **High 64001f** profile → **Share test pattern**.
3. In **Firefox**, then in **Safari**: open http://localhost:8080 → **Join**. Allow "local network" if macOS asks. The
   tile should play; click **Auto-switch test**, wait about 60 s, then **Copy results JSON**.
4. Repeat step 2 with **constrained baseline 42e01f**.

Expected, and what to record: whether Firefox shows video for High (the server reports `match partial`, i.e. it
forwards High under a Baseline payload type), and the auto-switch summary for each browser.
