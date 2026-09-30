# M1 design 02: the SFU (`internal/server/sfu`) and the load test

Status: draft for M1, 2026-09-29. `docs/PLAN.md` is the source of truth above this doc. Evidence: `spikes/s4-sfu`
(code and README), plus finding 7 of `spikes/s2-mac-vt/README.md`.

Written in parallel with `01-protocol.md`, `03-accounts-and-store.md`, `04-server-platform.md`, `05-web-client.md` and
`06-deploy-and-ci.md`, then reconciled by the integrator (see `README.md`, "Integration decisions"). The main changes:
share ids come from the hub; pub offers map m-sections to shares with 01's `tracks`, not the msid stream id;
negotiation uses 01's `gen`/`neg`; the hub owns share timeouts; ICE sockets and muxes come from 04's
`netx.Transport`; 01's `sfuplane` adapter is the only code that translates between this package and the wire.

Tags: **M1** = build now. **later (Mx)** = leave room now, build in milestone x.

## 1. Scope

In this doc (all M1):
- `internal/server/sfu`: rooms, participants, connections, shares, the two `webrtc.API`s (built on 04's
  `netx.Transport`), forwarding, layer selection, downlink adaptation, codec policy, guards, stats, the
  connection-test probe PCs.
- `internal/server/sfu/sfutest`: in-process harness, Go viewer, UDP fault injection. Used by the SFU tests and by
  the load test.
- `internal/media/fake`: the fake media source (test pattern and tones).
- `internal/client/publish`, M1 subset: the Go RTP publisher that tests and the load test use. M2 grows it into the
  native client core.
- `cmd/isshoni-loadtest`.

Not in this doc: the wire format, WebSocket sessions and the `sfuplane` adapter (01); accounts and REST (03); config
loading, TLS, the 443 first-byte mux, the ICE sockets and muxes, public-IP detection, the Prometheus endpoint, the
connection-test REST endpoint and doctor (04); browser code (05); packaging and CI jobs (06).

Not in M1: TWCC/GCC on the downlink (v2), embedded TURN, codecs other than H.264 and Opus, E2EE, WHIP, multi-node
(all later). Recording: never.

## 2. Decisions at a glance

| Topic | Decision | Why |
|---|---|---|
| Model | SFU → Room → Participant → Conn (one per signaling connection, max 2 PCs). Shares belong to the Participant; their media comes from one Conn's publish PC | Matches the plan's roles. A desktop app's Go publisher and its SPA viewer are one person |
| Driving | `signal` calls `*sfu.Conn` methods through 01's `sfuplane` adapter (01 §15.4). The SFU answers through a per-connection `Signaler` and room-wide `RoomEvents`. The SFU never imports `signal` or `protocol` | Keeps the internal API free to change while the wire format stays N/N−1 compatible |
| Share lifecycle | The hub (01) creates share ids, enforces share limits and owns both 30 s share timeouts. The SFU reports media facts (`pending → live`, `stalled`) and never ends a share by itself | One owner for every state that clients see; no racing timers |
| Concurrency | One actor goroutine per Conn does all PeerConnection calls. Media runs on per-layer and per-DownTrack goroutines. No lock is held across Pion calls or callbacks | Pion calls back from its own goroutines. An actor removes the deadlock and ordering bugs the spike's shared mutexes invited |
| Simulcast rids | `"f"` (full) and `"q"` (preview). `"h"` is reserved for the M5 720p30 middle layer | One byte per RTP packet for the rid extension. Same convention as LiveKit and the spike |
| Keyframes | A layer switch or resume happens only on a packet carrying an SPS (single NAL or STAP-A). PLI to a publisher is throttled to 1 per 500 ms per layer | S4: switching elsewhere leaves the decoder without the new resolution's SPS/PPS |
| Retransmission | One packet cache per incoming layer, sized to about 1 s at the measured packet rate. NACKs from every viewer (video and audio) are served from it, as RTX when negotiated. No Pion NACK responder on the subscribe side | Memory does not grow with the number of viewers |
| Sender reports | Each DownTrack forwards the publisher's SR: NTP unchanged, RTP shifted by the DownTrack's current offset. The subscribe side has no SR generator | Keeps the publisher's capture clock end to end (A/V target under 45 ms) |
| Layer-switch timestamps | Aligned through the two layers' SRs. Wall-clock fallback | The output timeline stays continuous in capture time, not arrival time |
| Downlink | Explicit `subscribe.update` (high/low/off plus audio on/off), plus a server downgrade on REMB or loss (Galene-style) with timed trial upgrades and a notice to the viewer | REMB caps at about 1.5× the received rate, so an upgrade has to be tried |
| Codec | H.264 only. Profiles match on profile_idc plus the constraint byte; the level is ignored. There is a compatibility matrix (High ↔ Constrained High). Per-room policy `high` or `cb` from the viewers' decode caps | S4 findings 3–5, S2-VT finding 7 |
| Viewer without H.264 (fresh Firefox profile) | No video transceivers until its caps list H.264. Then the SFU rebuilds its subscribe PC | S4: the first join fails; a new PC picks up the downloaded decoder |
| Server candidates | Complete SDP (no server-side trickle). Client trickle accepted, filtered (§7.3) | Gathering is instant with muxes, and candidates can't overtake their SDP |
| Sockets and public IP | 04's `netx.Transport` owns the UDP/TCP sockets, the muxes, interface and IP filters and the `SetICEAddressRewriteRules` host rewrite; the SFU calls `Transport.Apply` on each `SettingEngine` | One owner for sockets (byte counting, buffers, doctor); srflx rewrite would bypass the 7882 mux (04 §7.5) |
| SRTP | AEAD_AES_128_GCM first, AES128_CM_HMAC_SHA1_80 second | Egress encryption is the main per-packet CPU cost. GCM uses AES-NI or ARMv8 AES |
| Fake media | Synthetic, codec-shaped H.264 for load. A small pure-Go I_PCM/P_Skip encoder when the video must decode. Opus from a committed 1 s asset | CGO-free and license-clean. The SFU never decodes, so load numbers stay accurate at almost no CPU (§15.1) |

## 3. From the S4 spike to M1

| S4 result or shortcut | M1 |
|---|---|
| Finding 1: Pion drops RTP written before DTLS is up | DTLS-ready gate per subscribe PC, plus a PLI on connect (§9.3) |
| Finding 2: simulcast receivers need `ReadSimulcastRTCP(rid)` | Layer RTCP loop (§9.1) |
| Finding 3: Firefox decodes only CB/Baseline | Per-room codec policy (§8.3) |
| Finding 4: Chrome on macOS needs High for hardware simulcast | Policy defaults to `high` and drops to `cb` only when a viewer needs it |
| Finding 5: Chrome advertises High as `640034` | `ProfileKey` = first 4 hex digits (§8.2). Pion's own fmtp match does the same |
| Finding 6: macOS Local Network privacy | Not the SFU's job (04 doctor, 05 connection test). 04's `netx.Transport` lists the advertised candidates |
| Finding 7: headless Chrome can't use `getDisplayMedia` | Go tests use `internal/media/fake`. Browser e2e uses a canvas source (05) |
| Fresh Firefox profile: "codec is not supported by remote" | Caps-driven video, rebuild on caps change, and a server-side guard in `Bind` (§8.5) |
| S2-VT finding 7: VideoToolbox High is `6400xx`, no B-frames | High ↔ Constrained High counts as compatible (§8.2) |
| Spike: one room, Peer = WebSocket | Rooms. `Conn` outlives a WebSocket (resume within 30 s) |
| Spike: every layer queued to every DownTrack | Per-DownTrack interest mask: packets go only to DownTracks that want that layer |
| Spike: sub API used Pion's NACK responder and SR generator | Shared cache and forwarded SRs (per-viewer copies and server-clock SRs removed) |
| Spike: padding dropped, leaving sequence gaps that viewers NACK forever | Munger closes in-order padding gaps (§9.4) |
| Spike: wall-clock timestamp offset on switch | SR-aligned offset with wall-clock fallback (§9.4) |
| Spike: forwarded a "partial" profile match anyway | Only compatible profiles are forwarded. Otherwise `codec_mismatch` |
| Spike: `AddTrack` made sendrecv transceivers | Sendonly transceivers, reused after a share ends (§9.3) |

## 4. Package layout (M1)

```
internal/server/sfu/
  doc.go          overview, lock order, goroutine table (copy of §5.4)
  sfu.go          SFU, New, Close, Ready, Join, room reads, StopShare, CloseRoom, ticker
  config.go       Config, Limits, defaults, validation
  api.go          the webrtc.API instances: pub, sub, probe (MediaEngines, interceptors, SettingEngine on top of
                  04's netx.Transport.Apply), remote-candidate filter
  room.go         Room: participants, shares, codec policy
  participant.go  Participant
  conn.go         Conn actor: command queue, Pion event queue, Resync, Close
  pubpc.go        publish PC: offer handling, validation, answer filtering, OnTrack → Layer
  subpc.go        subscribe PC: negotiation state machine, ICE restart, rebuild, DTLS gate
  subscription.go Subscription {video, audio DownTrack}, UpdateSubscriptions
  share.go        Share: lifecycle, layers, fan-out lists, keyframe requests, ShareInfo
  layer.go        Layer: RTP/RTCP read loops, SR store, rate and loss estimate, SPS info
  packetcache.go  per-layer ring cache
  downtrack.go    DownTrack (webrtc.TrackLocal): Bind, queue, writer, RTCP reader
  munger.go       seq/ts rewrite, epochs (the sequence map), SR-aligned switching, padding gaps
  rtx.go          NACK handling, RTX packets, retransmission budget
  pacer.go        per-DownTrack token bucket
  allocator.go    per-subscriber downlink controller
  uplink.go       UplinkPolicy (M1: admin cap and layer pausing; M2: native estimator)
  codec.go        ProfileKey, compatibility matrix, PT table, codec policy helpers
  h264.go         SPS detection (keyframe start), SPS parser (size), NAL helpers
  sdpcheck.go     publish-offer validation, subscribe-answer checks, Opus fmtp edit (pion/sdp)
  probe.go        connection-test probe PCs
  events.go       Signaler, RoomEvents, Event types
  errors.go       Error and codes
  stats.go        ShareInfo, ConnStats, Snapshot, Metrics
  *_test.go
internal/server/sfu/sfutest/   Harness, DirectSignaler, Viewer (+Recorder, checks), FaultConn
internal/media/fake/           Source, synthetic and decodable H.264, Opus asset, testdata/
internal/client/publish/       Publisher: simulcast TrackLocal with mid/rid, H.264/Opus packetizing, SRs, PLI
cmd/isshoni-loadtest/          main.go, auth.go, scenario.go, measure.go, report.go
```

Dependencies (all permissive): `pion/webrtc/v4` v4.2.x (v4.2.22 in S4), `pion/ice/v4`, `pion/interceptor`, `pion/rtp`,
`pion/rtcp`, `pion/sdp/v3`, 04's `internal/server/netx` (sockets and muxes), and `go.uber.org/goleak` (MIT, tests
only). The SFU does **not** import Prometheus: 04 adapts `sfu.Metrics` (§13), so metrics stay optional in one place.

## 5. Object model

### 5.1 Entities and ownership

```
SFU ── shared: pubAPI, subAPI, probe APIs (all on 04's netx.Transport: UDP mux :7882, TCP muxes :443 and :7882),
       250 ms ticker
 └─ Room (in memory; created on the first Join, removed when it has no Conns and no shares)
     ├─ Participant (user in room; ≥1 Conn)
     │    └─ Conn (one signaling connection; role full|viewer|publisher|agent)
     │         ├─ pubPC  (client offers)  ──► Layers (TrackRemote per rid + audio) ─┐
     │         ├─ subPC  (server offers)  ◄── DownTracks ◄──── fan-out ──────────────┤
     │         └─ subscriptions[shareID] = {video DownTrack, audio DownTrack}        │
     └─ Share (owned by the Participant; media from exactly one Conn's pubPC) ───────┘
          ├─ layers[f|h|q]: Layer (+ packet cache)   audio: Layer (+ packet cache)
          └─ fan-out lists: video DownTracks, audio DownTracks (copy-on-write)
```

| Object | Owned by | Holds | Ends when |
|---|---|---|---|
| `Room` | SFU | participants, shares, codec policy | last Conn gone and no shares; `CloseRoom` |
| `Participant` | Room | conns, shares (≤4) | last Conn closed |
| `Conn` | Participant | ≤1 pubPC, ≤1 subPC, subscriptions, pending ICE, negotiation state | `Close` (signal: grace expired, leave, revocation, room closed); SFU shutdown |
| `Share` | Participant (source Conn referenced) | layers, audio layer, fan-out lists, state | `StopShare` from signal (01 owns the lifecycle and its timeouts); source Conn closed; `CloseRoom`; SFU `Close` |
| `Layer` | Share | `TrackRemote`, receiver, packet cache, latest SR, rates | its track's read fails (PC closed, transceiver stopped) |
| `Subscription` | subscriber Conn | video and audio `DownTrack`, requested quality | share ended; subscriber Conn closed |
| `DownTrack` | Subscription | queue, munger, binding to subPC sender, pacer, counters | subscription removed or subPC rebuilt (it re-binds) |

A Share survives a rebuild of its publish PC: the new PC's offer (`gen + 1`) lists the same `shareId`s in `tracks`,
the new tracks attach to the same Share, and viewers keep their transceivers. They see a short freeze and then a
keyframe.

### 5.2 Identifiers and naming

- `RoomID`, `ParticipantID`, `ConnID`, `UserID`: opaque strings assigned by signal/store (01, 03). The SFU only
  compares them.
- `ShareID`: assigned by the signal hub (01 §4.1): `"s_"` plus 16 lowercase base32 characters from `crypto/rand`
  (80 bits), for example `s_q7m2x9c4v8b1n5k3`, passed in `StartShareParams.ID`. Unique across restarts, so stale
  client IDs never match a new share. It is a valid msid token.
- Subscribe side SDP: msid stream id = `ShareID`, track id = `"v-"+ShareID` or `"a-"+ShareID`. Every sub offer also
  returns its `tracks` binding (mid → share, kind), which the viewer uses to map `ontrack` (01 §9 rule 4); the msid
  is a debugging aid.
- Publish side: every pub offer carries 01's `tracks` binding (mid → share, kind) for each sending m-section. The SFU
  maps tracks to shares by that binding, read afresh on every offer. A rebuilt pub PC has new mids but lists the same
  `shareId`s, so its tracks attach to the existing Shares. The msid stream id the browser chose is ignored.
- Rids: `f` = full (source, ≤1080p60 by default), `q` = preview (360p15, about 0.3 Mbps), `h` = reserved (M5). A video
  m-line without rids is a single `f` layer. Any other rid is rejected.

### 5.3 Lifecycles

**Share** (M1). The SFU reports media facts; the hub (01 §4.4) owns the lifecycle, the 30 s `starting` and `stalled`
timeouts (`media_timeout`) and the end reasons:

```
StartShare ─► pending ──(first SPS keyframe on any video layer)──► live ◄──────────────► stalled
                                                                         pub PC not connected ≥ 2 s, or its
                                                                         video layers are gone (pub PC closed,
                                                                         failed or replaced by a new gen); back to
                                                                         live on the first keyframe of the tracks
                                                                         a new pub offer binds to this share
Any state, only on a call from signal: StopShare(r) · source Conn Close(r) · CloseRoom(r) · SFU Close(server_shutdown)
  → the Share is removed and ShareEnded(r) fires; r is 01's EndReason string
```

`RoomEvents.ShareUpdated` fires on every state change and on layer, profile or audio changes while live. The hub
turns them into `live`/`stalled` in `room.state`, sends Web Push ("X started streaming") on the first `live` only,
so aborted shares never notify (04 push, 01), and ends shares that stay `pending` or `stalled` for 30 s.

**Subscribe PC negotiation** (server is always the offerer on `sub`; the client on `pub`; so there is never glare):

| State | Event | Action → next |
|---|---|---|
| `idle` | change (track added/removed, ICE restart asked) | debounce 50 ms (01 §9), `CreateOffer` → `SetLocalDescription` → wait for gathering (≤2 s) → `SendOffer(sub, gen, neg=n, sdp, tracks)` → `offering` |
| `offering` | change | set `dirty` |
| `offering` | answer with this `gen` and `neg == n` | check answer (§8.5), `SetRemoteDescription`, flush buffered candidates → `idle`; if `dirty`, offer again (`neg + 1`) |
| `offering` | answer with another `gen` or `neg` | ignore, log at debug |
| `offering` | 15 s, no answer | send the same offer again (same `gen` and `neg`) |
| `offering` | 30 s, no answer | rebuild the sub PC (`gen + 1`), `ErrorEvent{sfu.negotiation_timeout}` |
| any | `Resync()` | if `offering`, send the same offer again; ICE-restart a sub PC that isn't `connected` (01 §10.5) |
| any | `ResetPC(sub)`, a codec rebuild (§8.5) or a fatal error | close PC; new PC with `gen + 1`, `neg = 1`; add all DownTracks again; offer |

`gen` and `neg` are 01's counters (01 §9): per Conn and PC kind, `gen` counts PC generations (the SFU increments it
for `sub`, the client for `pub`) and `neg` counts offers within a `gen`. A pub offer with a higher `gen` replaces the
pub PC; one with a lower `gen` is refused (01 answers `stale_negotiation`). A repeated pub `neg` gets the stored last
answer again.

**PeerConnection state** (both kinds):
- New PC or ICE restart: a 10 s handshake timer (ICE + DTLS to `connected`). For a new PC, expiry closes it and sends
  `PCStateEvent{failed, "handshake_timeout"}`. For an ICE restart it only sends the event.
- `connected`: sub PC opens the DTLS gate and sends a keyframe request for every video DownTrack.
- `disconnected`: nothing. The client drives restarts (01/05: ICE `disconnected` for 3 s → restart).
- `failed`: send `PCStateEvent`, start a 30 s PC grace. With no restart or reset by then, close the PC. For pub, its
  shares are `stalled` until a new pub offer binds their tracks (or the hub's 30 s timeout ends them).

**Conn**: `active` → (`Close`) → `closed`. There is no "detached" state in the SFU: a WebSocket drop is invisible to it.
Signal keeps the `Signaler` alive, calls `Resync()` on resume and `Close(disconnected)` after the 30 s grace (plan).

### 5.4 Goroutines and locks

| Goroutine | Count | Does | Waits on |
|---|---|---|---|
| Conn actor | 1 per Conn | every signaling operation and every Pion PC call (SRD, SLD, CreateOffer/Answer, AddTrack, RemoveTrack, Close); handles Pion callbacks | command queue (cap 64) + Pion event queue (unbounded) |
| Layer RTP reader | 1 per incoming layer | `Read` → parse → cache insert → fan-out to interested DownTracks | Pion read |
| Layer RTCP reader | 1 per incoming layer | `ReadSimulcastRTCP(rid)` / `ReadRTCP()` → SR store, SR items to DownTracks | Pion read |
| DownTrack writer | 1 per DownTrack | RTX first, then munge, pace, write RTP, forward SRs | its queues |
| DownTrack RTCP reader | 1 per DownTrack | PLI/FIR, NACK, REMB, RR from the viewer | `RTPSender.ReadRTCP` |
| Ticker | 1 per SFU | 250 ms: event debounce. 1 s: rates, cache sizing, stalled checks, allocator ticks, codec-policy timers, uplink policy | time |
| Probe | 1 per probe PC | 20 s lifetime | timer |

Rules:
- Lock order: `SFU.mu` → `Room.mu` → `Share.mu` → `DownTrack.mu`. Each is held briefly and never across a Pion call,
  a `Signaler`/`RoomEvents` call or a blocking channel send.
- Pion callbacks (`OnTrack`, `OnConnectionStateChange`, `OnICEConnectionStateChange`) only append to the Conn's
  unbounded event queue. They never block, so a Pion call made by the actor can't deadlock on its own callback.
- Signal calls block on the actor for at most 5 s (`ctx`). A full command queue after that returns `sfu.busy`
  (retryable).
- Fan-out lists (`Share.video`, `Share.audio`) are `atomic.Pointer[[]*DownTrack]`, copied on write, so the read path
  takes no lock.
- DownTrack munger state is guarded by `DownTrack.mu`. The writer goroutine takes it once per packet (uncontended).
  Control calls (`setRequested`, `setCap`) take it briefly.
- `Signaler` and `RoomEvents` implementations must not block and must not call into the SFU synchronously.

## 6. The signal ↔ sfu interface (M1)

### 6.1 Go API

```go
package sfu

type (
	RoomID        string
	ParticipantID string
	ConnID        string
	UserID        string
	ShareID       string     // "s_" + 16 base32 chars, assigned by the signal hub (01) and passed in
	ProfileKey    string     // "42e0" | "4200" | "4d00" | "640c" | "6400" (first 4 hex digits of profile-level-id;
	                         // 01's CodecKey without the "h264/" prefix)
)

type Role uint8       // RoleFull (publish+subscribe) | RoleViewer | RolePublisher (M2 desktop core) | RoleAgent (M4)
type ClientKind uint8 // ClientWeb | ClientDesktop | ClientMobile (later)
type PCKind uint8     // PCPub (client offers) | PCSub (server offers)
type Quality uint8    // QualityOff | QualityLow | QualityHigh; later (M5): QualityMedium ("h")
type Preset uint8     // PresetAuto | PresetGame | PresetMovie | PresetText
type SourceKind uint8 // SourceUnknown | SourceScreen | SourceWindow | SourceTab
type ShareState uint8 // SharePending | ShareLive | ShareStalled (media facts only; §5.3)
type EndReason string // 01's protocol.EndReason values, passed through: "stopped" "left" "disconnected"
                      // "media_timeout" "room_closed" "server_shutdown" ("kicked" reserved)
type SubReason string // "" "bandwidth" "server_limit" (later) "codec_mismatch" "decoder_unavailable"
                      // "decoder_failed" "no_preview_layer" "no_layer"; 01 §15.4 maps them to the wire

type DecodeCaps struct {
	H264 []ProfileKey // what the viewer can decode; empty = no H.264 decoder (yet)
}

// ---- construction (called by 04's wiring, internal/server) ----
func New(cfg Config, deps Deps) (*SFU, error) // builds the APIs on cfg.Transport (sockets are 04's, already bound)
func (s *SFU) Close() error                    // ends every share (server_shutdown), closes PCs; the Transport is 04's
func (s *SFU) Ready() error                    // nil once the APIs are built (for /readyz)

type Deps struct {
	Events RoomEvents   // 01's sfuplane
	Logger *slog.Logger // component=sfu
}

// ---- rooms and connections (called by signal) ----
type JoinParams struct {
	Room        RoomID
	Participant ParticipantID
	User        UserID
	Conn        ConnID
	Role        Role
	Client      ClientKind
	Decode      DecodeCaps
	Signaler    Signaler // stable for the Conn's life (survives WebSocket resumes)
}
func (s *SFU) Join(p JoinParams) (*Conn, error)
func (s *SFU) Shares(room RoomID) []ShareInfo       // live snapshot, sorted by StartedAt
func (s *SFU) Share(id ShareID) (ShareInfo, bool)
func (s *SFU) CodecPolicy(room RoomID) ProfileKey    // "6400" (high) or "42e0" (cb)
func (s *SFU) StopShare(id ShareID, r EndReason) error // any Conn; the hub normally uses Conn.StopShare
func (s *SFU) CloseRoom(room RoomID, r EndReason)
func (s *SFU) SetLimits(l Limits)                    // admin soft limits, applied live (wiring: 03 settings OnChange)
// later: func (s *SFU) SetRoomLimits(room RoomID, l RoomLimits)
func (s *SFU) Snapshot() Snapshot                    // §13
func (s *SFU) Metrics() Metrics                      // §13
func (s *SFU) Probe(ctx context.Context, user UserID, t ProbeTransport, offerSDP string) (string, <-chan ProbeResult, error) // §7.6

// ---- per connection (called by 01's sfuplane from the hub's connection actor; serialized by the Conn actor) ----
func (c *Conn) ID() ConnID
// HandleOffer applies a pub offer and returns the answer SDP synchronously (gathering is instant with muxes).
// A higher gen replaces the pub PC (the client rebuilt it); tracks binds every sending m-section to a share.
func (c *Conn) HandleOffer(ctx context.Context, pc PCKind, gen, neg uint32, sdp string, tracks []TrackBinding) (answerSDP string, err error) // PCPub only
func (c *Conn) HandleAnswer(ctx context.Context, pc PCKind, gen, neg uint32, sdp string) error // PCSub only
func (c *Conn) AddICECandidate(ctx context.Context, pc PCKind, gen uint32, cand webrtc.ICECandidateInit) error // filtered, §7.3
func (c *Conn) RestartICE(ctx context.Context, pc PCKind) error // PCSub only (for pub the client re-offers)
func (c *Conn) ResetPC(ctx context.Context, pc PCKind) error    // PCSub: close and rebuild with gen + 1 (client asked)
func (c *Conn) ClosePC(ctx context.Context, pc PCKind) error    // the client closed a PC on purpose (pc.close)
func (c *Conn) StartShare(ctx context.Context, p StartShareParams) (ShareParams, error)
func (c *Conn) UpdateShare(ctx context.Context, id ShareID, u ShareUpdate) (ShareParams, error)
func (c *Conn) StopShare(ctx context.Context, id ShareID, r EndReason) error
func (c *Conn) UpdateSubscriptions(ctx context.Context, items []SubscriptionUpdate) ([]error, error)
func (c *Conn) SetDecodeCaps(ctx context.Context, caps DecodeCaps) error
func (c *Conn) Resync()              // WebSocket resumed: re-send an outstanding offer and current states
func (c *Conn) Stats() ConnStats
func (c *Conn) Close(r EndReason)    // idempotent
func (c *Conn) Done() <-chan struct{}

type StartShareParams struct {
	ID     ShareID    // from the hub (01)
	Preset Preset     // sets the encodings and the Opus maxaveragebitrate in the publish answer (§8.6)
	Audio  bool       // the publisher will send one audio track
	Source SourceKind // informational, copied into ShareInfo
}
type ShareUpdate struct{ Preset *Preset } // new encodings at once; audio bitrate on the next publish offer

// TrackBinding is 01's TrackRef: one m-section (by mid) carrying one share's video or audio.
type TrackBinding struct {
	MID   string
	Share ShareID
	Kind  webrtc.RTPCodecType
}

// ShareParams tells the sharer how to encode (01's protocol.ShareParams without the wire types). Numbers: §8.6.
type ShareParams struct {
	Profile      ProfileKey       // the room's codec policy: put first in the publisher's codec preferences
	Encodings    []EncodingParams // f first, then q
	AudioBitrate int              // Opus target in bit/s (the pub answer's maxaveragebitrate)
}
type EncodingParams struct {
	RID          string // "f" | "q"
	Active       bool
	MaxBitrate   int    // bit/s, after the admin cap (Limits.MaxShareKbps)
	MaxFramerate int
	MaxPixels    int    // pixel budget, aspect-ratio neutral
}

type SubscriptionUpdate struct {
	Share ShareID
	Video Quality // high = focused, low = thumbnail, off = hidden
	Audio bool    // audio follows focus: normally true only for the focused share
}
```

`UpdateSubscriptions` applies items in order. The first slice holds one error per item (nil = applied), for example
`sfu.share_not_found` for a share that just ended. The second is a Conn-level error (role, closed, busy). Creating
subscriptions triggers one debounced renegotiation for the whole batch. Changing quality never renegotiates.

### 6.2 SFU → signal

```go
// Implemented by 01's sfuplane peer (one per Conn). Must not block (enqueue or drop) and must not call the SFU
// synchronously. While the WebSocket is down signal may drop calls: Resync() re-sends what matters.
// Pub answers are not here: HandleOffer returns them.
type Signaler interface {
	SendOffer(pc PCKind, gen, neg uint32, sdp string, tracks []TrackBinding) // PCSub in M1
	SendEvent(ev Event)
}

type RoomEvents interface {
	ShareUpdated(room RoomID, s ShareInfo) // debounced to at most one per share per 250 ms; state changes are never merged away
	ShareEnded(room RoomID, s ShareInfo, r EndReason)
	CodecPolicyChanged(room RoomID, p ProfileKey)
}

type Event interface{ isEvent() }

type SubscriptionStateEvent struct { // to the subscriber; only when Forwarded or Reason changes
	Share     ShareID
	Requested Quality
	Forwarded Quality   // what the SFU actually sends
	Audio     bool      // audio actually forwarded
	Reason    SubReason // why Forwarded < Requested ("" when equal)
}
type CodecPolicyEvent struct{ Profile ProfileKey } // to Conns that may publish: re-offer with this profile first
type QualityHintEvent struct {                     // to the publishing Conn
	Share      ShareID
	MaxBitrate int         // bps; 0 = no cap. M1: admin cap. M2: native uplink estimator
	Layers     []LayerHint // M1 (stretch, §11): pause/resume layers nobody watches
}
type LayerHint struct {
	RID    string
	Active bool
}
type PCStateEvent struct {
	PC     PCKind
	State  string // "connected" "disconnected" "failed" "closed"
	Reason string // "" | "handshake_timeout" | "pc_grace_expired"
}
type ErrorEvent struct {
	Err   *Error
	Scope string // "pc.pub" | "pc.sub" | "share" | "subscription"; sfuplane maps it to 01's scope and pc fields
}
```

`ShareInfo`:

```go
type ShareInfo struct {
	ID          ShareID
	Room        RoomID
	Participant ParticipantID
	User        UserID
	Conn        ConnID
	Source      SourceKind
	Preset      Preset
	State       ShareState      // ended shares are never listed
	Profile     ProfileKey      // current H.264 profile of the video ("" until known)
	Layers      []LayerInfo     // sorted f, h, q
	Audio       bool            // an audio track is attached
	Viewers     []ParticipantID // sorted; forwarding now (dashboard). room.state watchers come from the hub's
	                            // desired subscriptions instead (01 §4.1)
	StartedAt   time.Time
	LiveAt      time.Time       // zero until the first keyframe
}
type LayerInfo struct {
	RID           string
	Width, Height int     // from the last SPS
	FPS           float64 // measured
	Bitrate       int     // bps, 2 s EWMA
	Active        bool    // packets in the last 2 s
}
```

### 6.3 Errors

```go
type Error struct {
	Code      string // stable; never sent on the wire: 01's sfuplane maps it to a protocol ErrorCode (01 §15.4)
	Retryable bool
	msg       string // logs only, never sent
}
func (e *Error) Error() string
```

| Code | Returned by | Retryable | Meaning |
|---|---|---|---|
| `sfu.closed` | any Conn method | no | Conn or SFU closed |
| `sfu.busy` | any Conn method | yes | actor queue full for 5 s |
| `sfu.role_forbidden` | StartShare, HandleOffer(pub), UpdateSubscriptions | no | role can't do that |
| `sfu.bad_pc` | HandleOffer, HandleAnswer, RestartICE | no | method not valid for that PC kind (e.g. an offer for `sub`), or the PC doesn't exist |
| `sfu.bad_sdp` | HandleOffer, HandleAnswer | no | unparsable SDP, data channel in a media PC, >8 m-lines, a pub offer >64 KiB or a sub answer >256 KiB (01 §3.3) |
| `sfu.no_h264` | HandleOffer | no | the video m-line offers no H.264 packetization-mode=1 (browser without H.264 encoder) |
| `sfu.bad_rid` | HandleOffer | no | a rid outside {f,h,q}, or more than 3 |
| `sfu.unknown_track` | HandleOffer | no | a sending m-section missing from `tracks`, or a binding to a share that isn't this Conn's |
| `sfu.stale_answer` | HandleAnswer | no | `gen`/`neg` don't match the outstanding offer (sfuplane drops it silently) |
| `sfu.pc_limit` | HandleOffer | no | a third PC or a second PC of the same kind |
| `sfu.pc_rate_limited` | HandleOffer, ResetPC | yes | more than 10 PC creations per Conn per minute |
| `sfu.share_not_found` | StopShare, UpdateShare, UpdateSubscriptions item | no | unknown or ended share |
| `sfu.not_owner` | Conn.StopShare, UpdateShare | no | the share belongs to another Conn (the hub always calls the publishing Conn) |
| `sfu.too_many_shares` | StartShare | no | defense in depth: 4 per participant (the hub enforces 4 per user and the room soft limit first, 01 §8.7) |
| `sfu.too_many_subscriptions` | UpdateSubscriptions | no | guard: 256 per Conn, 64 items per call |
| `sfu.negotiation_timeout` | ErrorEvent | yes | sub answer missing for 30 s; the sub PC was rebuilt |
| `sfu.probe_limit` | Probe | yes | 3 probes per user or 20 per server already running |
| `sfu.transport_disabled` | Probe | no | `tcp443` requested in `tls.mode=off` (04 answers 409 `transport_disabled`) |
| `sfu.internal` | any | yes | unexpected Pion error (logged with details) |

01 owns the error envelope (`error{code, retryable, scope}`); `sfuplane` maps these codes to 01's catalog (01 §15.4).

### 6.4 How signal drives it (wire names are 01's)

01 owns every message name and field. 01's `sfuplane` (01 §15.4) is the only translator; this table is the short
version for SFU implementers:

| Wire (01) | `sfuplane` calls | SFU does |
|---|---|---|
| `hello.caps.decode` (`h264/<4 hex>` CodecKeys), `caps.update` | `SFU.Join(JoinParams{Decode})`, `Conn.SetDecodeCaps` | codec policy (§8.3), caps-driven video (§8.5) |
| `share.start {kind, preset, audio, ref}` → `ok ShareParams` | `Conn.StartShare({ID: hub's shareId, Preset, Audio, Source: kind})` | creates the Share (`pending`), returns encodings and codec (§8.6) |
| `share.update {shareId, preset}` | `Conn.UpdateShare` | new `ShareParams` |
| `share.stop`, `pc.close{pub}`, a pub offer without a share's tracks | `Conn.StopShare(id, stopped)` (the hub decides) | removes the Share |
| `pc.offer {pc: pub, gen, neg, sdp, tracks}` → `pc.answer` | `Conn.HandleOffer(PCPub, gen, neg, sdp, tracks)` → answer | §8.4 |
| `pc.offer {pc: sub, …}` ← | `Signaler.SendOffer(PCSub, gen, neg, sdp, tracks)` | §5.3 |
| `pc.answer {pc: sub, gen, neg, sdp}` | `Conn.HandleAnswer(PCSub, gen, neg, sdp)` | bind, DTLS gate, PLI, media |
| `pc.ice {pc, gen, candidate}` | `Conn.AddICECandidate(pc, gen, init)` | filtered (§7.3), buffered before the remote description |
| `pc.restart {pc: sub, mode: ice}` / `{mode: rebuild}` | `Conn.RestartICE(PCSub)` / `Conn.ResetPC(PCSub)` | ICE-restart offer / new sub PC `gen + 1` |
| `pc.restart {pc: pub, mode: rebuild}` ← | from `PCStateEvent{PCPub, failed}` | the client rebuilds and offers `gen + 1` |
| `subscribe.update {subs: [{shareId, video, audio}]}` | `Conn.UpdateSubscriptions` | DownTracks, one debounced offer |
| `subscribe.status` ← | from `SubscriptionStateEvent` | §10 |
| `quality.hint {shareId, reason, codec?, encodings?, maxBitrate?}` ← | from `CodecPolicyEvent` and `QualityHintEvent` | §8.3, §11 |
| `stats` ← (while `stats.watch`) | from `Conn.Stats()` | §13 |
| `room.state` shares ← | built by the hub from its own share records plus `RoomEvents.ShareUpdated` | §5.3 |

### 6.5 Reconnect and failure cases

| Case | Client (05) | Signal (01) | SFU |
|---|---|---|---|
| WebSocket drops, media fine | reconnect with jittered backoff, `hello{resumeToken}` | same session → `Conn.Resync()` | re-sends an outstanding sub offer (same `gen`/`neg`), current `SubscriptionStateEvent`s, `CodecPolicyEvent`, `QualityHintEvent`s; ICE-restarts a sub PC that isn't connected |
| WebSocket gone for 30 s | – | `Conn.Close(disconnected)` | removes its shares and DownTracks, closes PCs |
| ICE `disconnected` ≥3 s on sub | `pc.restart{pc: sub, mode: ice}` | `Conn.RestartICE(PCSub)` | ICE-restart offer, same `gen`, next `neg` (after a pending answer if one is outstanding) |
| ICE `disconnected` ≥3 s on pub | re-offer with fresh ICE credentials, same `gen` | `Conn.HandleOffer(PCPub, gen, neg+1, …)` | normal answer. Pion restarts ICE; SSRCs stay |
| ICE `failed` on sub | `pc.restart{pc: sub, mode: rebuild}`, then answers the new offer | `Conn.ResetPC(PCSub)` | closes the PC, builds a new one (`gen + 1`) with all subscriptions, offers |
| ICE `failed` on pub | new PC, offer with `gen + 1` and the same `shareId`s in `tracks` | `Conn.HandleOffer(PCPub, gen+1, 1, …)` | old layers end → shares `stalled` → new tracks attach → `live`. Viewers don't renegotiate |
| UDP blocked | nothing | nothing | ICE picks TCP 443 (or 7882) from the same candidate list |
| Viewer gains H.264 (Firefox download done) | `caps.update` | `Conn.SetDecodeCaps` | rebuilds the sub PC with video (§8.5) |
| Server restart | rejoin the last room, re-publish with `replaces` | new Conn | new shares and new share ids. Nothing persists in the SFU |

## 7. Networking (M1)

### 7.1 Sockets and muxes: 04's `netx.Transport`

04 owns the ICE sockets (04 §7.1–§7.6): one UDP socket per usable local address on 7882 (8 MiB buffers, read back,
byte-counted), `ice.MultiUDPMuxDefault`, the 7882/tcp listener and the 443 first-byte mux's ICE side, each wrapped
in `ice.TCPMuxDefault` and combined in `ice.MultiTCPMuxDefault`, the interface and IP filters, public-IP detection and
the `ICEAddressRewriteRules`. The SFU receives the built `*netx.Transport` in `Config.Transport` and:
- calls `Transport.Apply(se)` on every `SettingEngine` it builds (pub, sub and the probe APIs), then adds the settings
  in §7.3;
- uses `Transport.UDPMux`, `Transport.TCPMux443` and `Transport.TCPMux7882` separately for the per-transport probe
  APIs (§7.6);
- never binds a socket itself. Tests inject `sfutest.FaultConn` through 04's `netx.TransportOptions.PacketConns`.

Consequences that stay true (04 documents them for operators): the published port must equal the listening port
(Docker `443:443`, `7882:7882/udp`, `7882:7882/tcp`), because Pion advertises the listener's port and the v4.2
`ICEAddressRewriteRule` can't rewrite ports; and many goroutines may call `WriteTo` on the mux sockets. Later:
`sendmmsg` batching (pion/transport batchconn) if egress passes about 50k pps.

### 7.2 Local IPs and the public IP

Owned by 04 (§7.4 public-IP detection and NAT classification, §7.5 rewrite rules and filters). What the SFU relies
on: in the usual VPS case the public address is on an interface and no rule applies; behind 1:1 NAT or Docker's bridge
the local address is replaced by the public one; on a home server the public one is appended, so LAN clients still
connect directly. `SetNAT1To1IPs` and srflx rewrites are never used.

### 7.3 SettingEngine (shared by the pub and sub APIs; the probe APIs differ only in network types and muxes)

| Setting | Value | Why |
|---|---|---|
| `Transport.Apply(se)` (04) | UDP/TCP muxes, network types, interface and IP filters, rewrite rules, loopback (dev), mDNS disabled | §7.1 |
| `DisableActiveTCP(true)` | | the server never dials out |
| `SetLite(false)` | full ICE | S4 tested full ICE; no reason to change |
| Remote-candidate filter (in `AddICECandidate`) | drop loopback (unless `network.include_loopback`), link-local, multicast and unspecified addresses always; drop RFC 1918, CGNAT and ULA addresses unless the server itself advertises a private address (04's LAN/Append case) | 01 §17: a client must not make the server probe internal networks. Client addresses are learned as prflx from incoming checks anyway |
| `SetICETimeouts(5 s, 15 s, 2 s)` | disconnected, failed, keepalive | the client drives restarts after 3 s; the server only cleans up |
| `SetPrflxAcceptanceMinWait(300 ms)`, `SetSrflxAcceptanceMinWait(0)`, `SetHostAcceptanceMinWait(0)` | | when controlling (sub PC), browser pairs are prflx. Pion's default 1 s wait delays the first frame. 300 ms lets a UDP pair win over TCP |
| `SetDTLSConnectContextMaker` | 10 s timeout | plan guard |
| `SetSRTPProtectionProfiles` | `SRTP_AEAD_AES_128_GCM`, `SRTP_AES128_CM_HMAC_SHA1_80` | cheaper per-packet encryption |
| receive MTU | Pion default (1460) | |

### 7.4 ICE-TCP on 443

04's `netx.PortMux` hands non-TLS connections (first byte `0x00`–`0x02`) to an `ice.TCPMuxDefault` inside
`netx.Transport` (04 §7.2–§7.3), with a 10 s first-byte and first-STUN timeout and per-IP limits. The SFU sees only
the resulting `ice.TCPMux`. There is no 443 listener in `tls.mode=off`.

### 7.5 Diagnostics

04's doctor and dashboard read the advertised candidates, effective socket buffers and warnings from `netx.Transport`
(04 §7.3, §13.2). The SFU adds only `Ready()` (APIs built) for `/readyz`, `Snapshot()` and `Metrics()` (§13).

### 7.6 Connection-test probe (M1; the REST endpoint is 04 §7.7, the UI 05 §14.2)

The setup wizard's connection test (and "Test my connection" for any signed-in user) runs real ICE checks per
transport. The SFU provides the probe PCs:

```go
type ProbeTransport string // "udp" | "tcp443" | "tcp7882" (tcp443 is unavailable in tls.mode=off)
type ProbeResult struct {
	Transport ProbeTransport
	Connected bool
	Selected  string        // "udp 203.0.113.5:7882" (server side of the nominated pair)
	RTT       time.Duration // from the candidate-pair stats
	Err       string        // "timeout" | "closed"
}
// offerSDP: a browser offer with one data channel (m=application) and no media. The answer is complete (no trickle)
// and holds only candidates of the requested transport. Browser candidates are not needed: the browser's checks
// reach the public address and the server learns prflx.
func (s *SFU) Probe(ctx context.Context, user UserID, t ProbeTransport, offerSDP string) (answerSDP string, res <-chan ProbeResult, err error)
```

- Three extra `webrtc.API`s built at startup from `Transport.Apply`, each restricted to one transport: UDP only
  (`Transport.UDPMux`); TCP 443 only (`Transport.TCPMux443`, absent in off mode → `Probe` returns
  `sfu.transport_disabled`); TCP 7882 only (`Transport.TCPMux7882`). Each has an empty MediaEngine and SCTP on.
- The probe PC **echoes every data-channel message** back unchanged, so the browser measures RTT itself (05 §14.2).
- ICE timeouts: disconnected 3 s, failed 8 s; DTLS 10 s. A probe PC lives at most 20 s, or until the channel closes.
  `res` gets one value, when it connects (RTT sampled 1 s later) or times out.
- Guards: 3 concurrent probes per user (the browser runs all three at once) and 20 per server (`sfu.probe_limit`,
  retryable; 04 answers 429). 04 adds 12 requests per minute per user.
- Access: any signed-in user (04). Only admins see provider fix text (05).

## 8. Codecs (M1)

### 8.1 MediaEngines, interceptors and header extensions

Both MediaEngines register the same static payload types. Pion matches remote PTs by fmtp and answers with the
offerer's PTs, so browsers' own PT numbers are fine on the publish side.

| PT | Codec | fmtp | RTX PT |
|---|---|---|---|
| 96 | H264/90000 | `level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f` | 97 |
| 98 | H264/90000 | `…;profile-level-id=42001f` | 99 |
| 100 | H264/90000 | `…;profile-level-id=4d001f` | 101 |
| 102 | H264/90000 | `…;profile-level-id=640c1f` | 103 |
| 104 | H264/90000 | `…;profile-level-id=64001f` | 105 |
| 111 | opus/48000/2 | pub: `minptime=10;useinbandfec=1;stereo=1;sprop-stereo=1;maxaveragebitrate=128000`; sub: same with `maxaveragebitrate=510000` | – |

| | Publish API (browser or native → SFU) | Subscribe API (SFU → viewer) |
|---|---|---|
| Video feedback | `goog-remb`, `ccm fir`, `nack`, `nack pli`, `transport-cc` | `goog-remb`, `ccm fir`, `nack`, `nack pli` (no transport-cc in v1) |
| Audio feedback | `nack`, `transport-cc` | `nack` |
| Header extensions | `sdes:mid`, `rtp-stream-id`, `repaired-rtp-stream-id` (video); `transport-wide-cc` (video, audio) | `abs-send-time` (video) only; REMB needs it without transport-cc |
| Interceptors | `nack.NewGeneratorInterceptor(GeneratorSize(2048), GeneratorInterval(50 ms))`, `report.NewReceiverInterceptor(ReceiverInterval(1 s))`, `twcc.NewSenderInterceptor()` | **none**: no NACK responder (the shared cache serves NACKs), no SR generator (SRs are forwarded), no stats interceptor |
| Not registered | NACK responder, report sender, stats interceptor (the SFU counts itself) | |

The pub registry is built by hand, not with `RegisterDefaultInterceptors`. S4's sub API used `ConfigureNack` and
`ConfigureRTCPReports`; M1 drops both.

### 8.2 Profile keys and compatibility

`ProfileKey(fmtp)` = the first 4 hex digits of `profile-level-id`, lowercased. The level is ignored (S4 finding 5;
Pion's `h264FMTP.Match` compares the same two bytes). `packetization-mode` must be `1`.

A viewer advertising the row key can receive a stream with the column key:

| Viewer decodes ↓ / stream → | `42e0` CB | `4200` B | `4d00` Main | `640c` CHigh | `6400` High |
|---|---|---|---|---|---|
| `42e0` | ✓ | ✓¹ | – | – | – |
| `4200` | ✓ | ✓ | – | – | – |
| `4d00` | ✓ | ✓¹ | ✓ | – | – |
| `640c` | ✓ | ✓¹ | ✓² | ✓ | ✓² |
| `6400` | ✓ | ✓¹ | ✓² | ✓² | ✓ |

¹ WebRTC Baseline streams (Chrome's OpenH264, native encoders) use no FMO, ASO or redundant slices, so they are CB in
practice.
² WebRTC encoders never emit B-frames (S4, S2-VT finding 7), so High and Main streams decode as Constrained High.
Exact matches are preferred over compatible ones. Every "compatible" cell must pass the real-decoder e2e checks in 05
before a release; a failing cell is removed from the table.

`bestPT(stream ProfileKey, negotiated []RTPCodecParameters) (PayloadType, ok)` returns an exact match first, then a
compatible one in the order `6400, 640c, 4d00, 42e0, 4200`.

### 8.3 Per-room codec policy

- **Viewer set**: Conns in the room with role `full` or `viewer` whose `DecodeCaps.H264` is non-empty. Conns with no
  H.264 at all are waiting for a decoder and don't count.
- **Policy**: `high` (`6400`) when every viewer's caps contain a key that can receive `6400` (that is, `6400` or `640c`).
  Otherwise `cb` (`42e0`).
- **Hysteresis**: `high` → `cb` at once (otherwise that viewer sees nothing). `cb` → `high` only after 60 s without any
  CB-only viewer, so a Firefox user who reconnects doesn't flip every sharer twice.
- On a change: `RoomEvents.CodecPolicyChanged`, and a `CodecPolicyEvent` to every Conn in the room with role `full`,
  `publisher` or `agent`. `sfuplane` turns it into `quality.hint{reason: codec, codec}` for each of that Conn's shares
  (01 §15.4). A browser sharer re-offers with that profile first in `setCodecPreferences` (05). A native sender (M2)
  re-offers and sends an IDR in the new profile. New shares get the current policy in `ShareParams.Profile`.
- A new viewer's `bestPT` fails for a share while the publisher still sends High: its video DownTrack forwards
  nothing and reports `SubscriptionStateEvent{Forwarded: off, Reason: codec_mismatch}`. It recovers on the first
  keyframe in a compatible profile (the PT change is handled like a layer switch, §9.4).

### 8.4 Publish answer

`HandleOffer(PCPub)` runs these steps in the actor:
1. **Validate** the parsed offer (`sdpcheck.go`), before anything is applied: ≤64 KiB, ≤8 m-lines, no
   `m=application`. Each sending m-line's mid appears in `tracks`, bound to a share of this Conn in `pending`, `live` or
   `stalled` (`sfu.unknown_track` otherwise). Per share: at most one video and one audio m-line. Video offers H.264
   packetization-mode=1. Rids ⊆ {f,h,q}, at most 3. A violation returns its §6.3 code and changes nothing.
2. Create the pub PC if needed, or replace it when `gen` is higher than the current one (PC guards, §12). Store the
   mid → share binding for `OnTrack`. Then `SetRemoteDescription(offer)` and flush buffered candidates.
3. **Filter codecs** on each video transceiver: `SetCodecPreferences` keeps, in the offerer's order, only the profiles
   the room policy allows. `high` allows all five; `cb` allows `42e0` and `4200`. Plus their RTX. The browser then
   has to encode an allowed profile. (Slice 10 verifies that Pion honours transceiver preferences in answers. If it
   doesn't, the fallback is to strip the other PTs from the offer before step 2.)
4. `CreateAnswer`, `SetLocalDescription(answer)`, wait for gathering (≤2 s).
5. **Opus bitrate per share**: in the copy sent to the client (not the local description), set `maxaveragebitrate` on
   each audio m-line to its share's preset: Movie 256000, otherwise 128000. The parameter only tells the remote
   encoder what to do, so Pion's local description can keep the default. `stereo=1;sprop-stereo=1` are already in the
   engine's fmtp. Then return the answer to the caller (`HandleOffer` is synchronous) and keep it as the stored last
   answer for a repeated `neg`.

### 8.5 Viewers without H.264 (fresh Firefox profile, S4)

- **Caps-driven**: a Conn with empty `DecodeCaps.H264` gets audio DownTracks only. Each subscription reports
  `Forwarded: off, Reason: decoder_unavailable`. 05 shows "Your browser is still getting its video decoder,
  retrying…" and polls `RTCRtpReceiver.getCapabilities('video')` every 5 s.
- **Recovery**: `SetDecodeCaps` going from no H.264 to some rebuilds the sub PC (close, new PC, all DownTracks, offer).
  A new Firefox PC picks up the newly installed OpenH264. Renegotiating the old PC may not, so it isn't relied on. It
  costs one audio gap of about 1 s, once per profile.
- **Server guard** (caps wrong or stale): before `SetRemoteDescription(answer)`, `sdpcheck` finds video m-lines whose
  answer contains no H.264. `DownTrack.Bind` never fails while at least one codec of its kind is negotiated: it returns
  the first negotiated codec and marks itself `unsupported`, so the whole negotiation doesn't fail the way it did in
  S4. The subscription then reports `decoder_unavailable`, and the SFU retries with a sub PC rebuild every 20 s, at
  most 9 times (3 min, matching 05's "Reload the page" hint). After that the reason is `decoder_failed` until the next
  `SetDecodeCaps`. If Pion calls `Bind` with an empty codec list for a rejected m-line and refuses a zero codec, the
  fallback is to `RemoveTrack` those video senders before SRD and re-offer. The fake-Firefox integration test pins
  this down.
- **One retry loop, on the server** (plan: "the SFU must retry a subscription that failed on the codec"). Clients only
  poll their capabilities and send `caps.update`; they never send `pc.restart` for codecs (01 §11.7). On the wire all
  three internal reasons are `reason: codec` (01 §15.4).

### 8.6 Encodings per preset (`ShareParams`, M1)

`StartShare`/`UpdateShare` return these. Browsers apply them with `setParameters` (05 §13.4); native senders (M2) use
the same numbers.

| Preset | `f` (full) | `q` (preview) | Audio (`AudioBitrate`, pub answer `maxaveragebitrate`) |
|---|---|---|---|
| Auto | ≤ 8 Mbps, 60 fps, `maxPixels` 2 073 600 (1080p) | 0.3 Mbps, 15 fps, `maxPixels` 230 400 (360p) | 128 000 |
| Game | as Auto | as Auto | 128 000 |
| Movie | as Auto | as Auto | 256 000 |
| Text | ≤ 8 Mbps, 30 fps, 2 073 600 | as Auto | 128 000 |

- `f.MaxBitrate = min(preset, Limits.MaxShareKbps × 1000)` when the admin cap is set; `q` is never capped below
  0.3 Mbps.
- Both encodings start `Active: true`. Layer pausing (§11) later sends `quality.hint` with `f` inactive.
- `Profile` is the room's codec policy (§8.3): `6400` or `42e0`.
- Advanced sizes (1440p60, 4K60) are later (M3, native senders); the table grows additively.

## 9. The media path (M1)

### 9.1 Ingress: `Layer`

```go
type Slot uint8 // SlotQ, SlotH, SlotF, SlotAudio; 1<<slot is the bit in DownTrack interest masks

type Layer struct {
	share   *Share
	slot    Slot
	rid     string
	kind    webrtc.RTPCodecType
	track   *webrtc.TrackRemote
	recv    *webrtc.RTPReceiver
	pubPC   *webrtc.PeerConnection // for PLI and REMB
	ssrc    uint32
	cache   *packetCache
	lastSR  atomic.Pointer[srInfo] // {ntp uint64, rtp uint32, arrival monotonic ns}
	lastPLI atomic.Int64
	lastRTP atomic.Int64           // monotonic ns
	stats   layerStats             // atomics: packets, bytes, lost, EWMA bitrate and pps, fps
	sps     atomic.Pointer[spsInfo] // width, height, profile from the last SPS
}

type packet struct { // immutable after insert; shared by the cache and every DownTrack queue
	layer    *Layer
	seq      uint16
	ts       uint32
	marker   bool
	pt       uint8
	profile  ProfileKey // from the PT via the pub PC's negotiated codecs (video)
	keyStart bool       // video: carries an SPS (single NAL or STAP-A). Audio: always true
	padding  bool       // no payload (probe padding): never cached, used only to close gaps
	payload  []byte     // exact-size copy, header extensions removed
	arrival  int64
}
```

- **RTP loop**: `track.Read(scratch)` into a reusable 1500-byte buffer, `rtp.Header.Unmarshal`, copy the payload into
  an exact-size slice (one allocation per incoming packet, none per viewer), build `packet`, `cache.insert`, then for
  each DownTrack in the share's list for that kind: if `dt.interest&slotBit != 0`, `dt.enqueue(p)`. Padding-only
  packets are enqueued as padding markers (no payload) to DownTracks currently forwarding this layer. The loop ends
  on a read error: the Layer detaches and the Share re-evaluates its state.
- Publisher RTX is unwrapped by Pion into the main track (repair stream), so recovered packets arrive late with their
  original seq and are cached and forwarded like any late packet.
- **RTCP loop**: simulcast video uses `recv.ReadSimulcastRTCP(rid)`, audio and single-layer video use
  `recv.ReadRTCP()` (S4 finding 2). This also keeps the pub interceptors (NACK generator, RR, TWCC) running. A
  `*rtcp.SenderReport` for this SSRC updates `lastSR` and enqueues an SR item to DownTracks forwarding this layer.
- **Keyframe start** (`h264.go`, kept from S4): the first NAL is an SPS (type 7), or a STAP-A (type 24) holds one. The
  STAP-A walk is bounds-checked and fuzzed.
- **SPS parser**: profile_idc, constraint flags, level and size (with cropping), for `LayerInfo` and the dashboard.
  It reuses the parser from `spikes/s2-mac-vt/h264.go` (our own code) and its tests.
- **Stats**, every 1 s from the ticker: bitrate and pps (2 s EWMA), FPS (distinct timestamps per second), loss (from
  seq gaps not filled within 1 s), `Active` = a packet within 2 s.

### 9.2 Packet cache (one per Layer)

- A ring indexed by `seq & mask` holding `*packet`, with each slot's seq for validation. Capacity is a power of two:
  `clamp(nextPow2(ceil(pps × 1 s)), 128, 8192)`. That is 1024 slots (about 1.2 MB) for an 8 Mbps full layer and 128
  for audio (2.5 s at 50 pps).
- Resizing is checked each second: grow at once when the wanted size is more than the current one; shrink only after
  10 s at a quarter of the current size or less. A resize copies the entries still in the window.
- `insert(p)`: ignored if `p.seq` is more than a capacity behind the highest seq (int16 arithmetic); otherwise it
  overwrites the slot. Out-of-order and late (RTX) packets land in their slots.
- `get(seq) *packet`: nil if the slot holds another seq or `arrival` is older than 1.5 s.
- Concurrency: one `sync.RWMutex` per cache. Writes come from the layer's RTP goroutine; reads from DownTrack RTCP
  goroutines on NACK.
- There are no per-viewer copies. Payloads are immutable and garbage-collected once neither the cache nor any queue
  holds them. Pooling with refcounts is a later optimization, only if the load test shows GC cost.

### 9.3 `DownTrack` (one per subscriber, share and kind)

Implements `webrtc.TrackLocal`: `ID()` = `v-`/`a-`+ShareID, `StreamID()` = ShareID, `RID()` = "", `Kind()`.

```go
type DownTrack struct {
	share    *Share
	sub      *Subscription
	kind     webrtc.RTPCodecType
	interest atomic.Uint32            // slot bits the writer wants (target and current)
	binding  atomic.Pointer[binding]  // nil until Bind, and after Unbind
	queue    chan item                // video 1024, audio 256; drop when full (counted)
	rtxQueue chan item                // 256; the writer drains it first
	mu       sync.Mutex               // guards m, requested, capQ, capReason
	m        munger
	requested, capQ Quality           // effective = min(requested, capQ)
	capReason SubReason
	pacer    pacer
	stats    dtStats                  // atomics
}

type binding struct {
	pc            *subPC // generation: ready flag (DTLS gate), *webrtc.PeerConnection for WriteRTCP
	writer        webrtc.TrackLocalWriter
	ssrc, rtxSSRC uint32
	ptFor         map[ProfileKey]uint8 // stream profile → viewer PT (bestPT); audio: Opus PT
	rtxPTFor      map[uint8]uint8      // viewer PT → its RTX PT, when negotiated
	absSendTimeID uint8                // 0 when not negotiated
	unsupported   bool                 // no H.264 negotiated (§8.5)
}
```

- **Transceivers**: to add a DownTrack, the actor first looks for a transceiver of the same kind with no sender whose
  current direction is `inactive` or `recvonly`. If there is one, `AddTrack` reuses it (Pion sets it to sendonly).
  Otherwise it calls `AddTransceiverFromTrack(dt, {Direction: sendonly})`. After a share ends, `RemoveTrack` sets the
  m-line inactive, and the next share reuses it, so the SDP doesn't grow over a 2-hour session. Viewers can never send
  media on the sub PC.
- **Bind**: builds `ptFor` from `ctx.CodecParameters()` (§8.2), finds the RTX PTs (`apt=`), and gets `ctx.SSRC()`,
  `ctx.SSRCRetransmission()` and the abs-send-time extension ID from `ctx.HeaderExtensions()`. It returns the codec for
  the share's current profile (or the first negotiated codec of its kind) and never an error while one exists. It
  stores the binding and requests a keyframe if the sub PC is already connected.
- **Unbind**: `binding = nil`. The writer drops packets until the next Bind.
- **DTLS-ready gate** (S4 finding 1): the writer forwards only when `binding.pc.ready` is true. The sub PC sets it on
  `connected` and then requests a keyframe for every video DownTrack. A rebuilt sub PC is a new `subPC` with its own
  gate, so a stale "ready" can never leak.
- **Writer loop**: `rtxQueue` first, then `queue`. For an RTP item:
  1. check gate and binding;
  2. `m.process(p, now)` → `(outSeq, outTS, ok)`, or wait for a keyframe (which triggers a throttled keyframe request
     on the target layer);
  3. build a **fresh** header: V=2, marker from upstream, PT = `ptFor[p.profile]`, seq, ts, SSRC; no CSRCs, no
     padding. None of the publisher's extensions (mid, rid, transport-cc) are copied. Add abs-send-time (the current
     time) when negotiated;
  4. `pacer.wait(len)` (video only);
  5. `writer.WriteRTP(&hdr, p.payload)` and update counters.

  For a padding marker: `m.skipPadding(p)`. For an SR item: §9.6.
- **RTCP reader** (`sender.ReadRTCP()`): Pion delivers a compound packet to every SSRC it names, so each DownTrack
  filters by its own SSRC.
  - `PictureLossIndication`, `FullIntraRequest` → `share.requestKeyframe(current slot)`.
  - `TransportLayerNack` → §9.5.
  - `ReceiverEstimatedMaximumBitrate` → `allocator.onREMB(bitrate, now)`. The same REMB reaches several DownTracks;
    the allocator keeps the latest.
  - `ReceiverReport` blocks for our SSRC → `fractionLost` → DownTrack loss EWMA and `allocator.onLoss`.
- **RTT** for the sub PC comes from the nominated ICE candidate pair (`pc.GetStats()`, sampled every 5 s). RR-based RTT
  is meaningless here because the forwarded SRs carry the publisher's NTP clock (§9.6).

### 9.4 Munger and the sequence map

The munger rewrites seq and ts so the viewer sees one continuous stream across layer switches, pauses, publisher
rebuilds and profile changes. An **epoch** starts at every discontinuity:

```go
type epoch struct {
	layer   *Layer     // Layer instance (not rid): a rebuilt publisher is a new epoch
	profile ProfileKey // a profile change (codec policy) is a new epoch too
	startS  uint16     // first own seq of the epoch
	startU uint16 // first upstream seq
	seqOff uint16 // own = upstream + seqOff
	tsOff  uint32 // own ts = upstream ts + tsOff
	at     int64
}
type munger struct {
	clock    uint32 // 90000 or 48000
	active   bool
	target   Slot
	epochs   [64]epoch // ring; the newest is current. This is the "own seq → (upstream seq, layer)" map
	n        int
	lastS    uint16    // highest own seq sent
	lastTS   uint32
	lastAt   int64
	lastU    uint16    // highest upstream seq processed in the current epoch
	started  bool
	switches int
}
```

Rules:
- **Start or switch**: a packet from the target slot while (no current epoch) or (current layer ≠ that Layer instance)
  or (the packet's profile ≠ the current epoch's profile) starts a new epoch only if `p.keyStart`. Until then the old
  layer keeps flowing, and a waiting DownTrack requests a keyframe (throttled). Audio packets are always `keyStart`.
- **New epoch seq**: `seqOff = lastS + 1 − p.seq` (continuous). First epoch: random `seqOff` and `tsOff`.
- **New epoch ts**, in order of preference:
  1. *Same Layer instance* (resume after `off`, audio after `off`, padding epoch): keep `tsOff`. Upstream ts already
     advanced in media time.
  2. *SR-aligned*: both the old and new layer have an SR less than 10 s old. Capture time of the keyframe:
     `t = ntp_n + int32(p.ts − rtp_n)/clock`. Old-timeline ts at `t`: `rtp_o + (t − ntp_o)·clock`. Then
     `outTS = that + tsOff_old` and `tsOff = outTS − p.ts`. Accepted if `0 < int32(outTS − lastTS) ≤ (elapsed + 1 s)·clock`.
  3. *Wall clock* (the S4 rule): `outTS = lastTS + max(1, elapsed·clock)`.
- **In-epoch packets**: `S = U + seqOff`, `T = ts + tsOff`. `lastS` and `lastTS` only move forward (int16/int32
  comparisons), so reordered packets keep their mapped seq.
- **Late packets** from before the current epoch's `startU`: look back through earlier epochs of the **same Layer**
  (padding epochs) and use the first with `startU ≤ U`. If a different layer is reached, drop the packet: its seq
  would collide with numbers already used.
- **Padding**: `skipPadding(p)` with `p.seq == lastU + 1` (in order) starts a new epoch with `seqOff − 1` (same layer,
  same `tsOff`), so the next media packet closes the gap. Out-of-order padding is ignored; the viewer may NACK that
  seq, it misses the cache, and nothing is lost.
- **Pause** (`off`): `active = false`. Nothing is forwarded. Resume follows the start rules (keyframe for video).
- **Lookup for NACK** `lookup(S) (epoch, ok)`: walk from the newest epoch back to the first with
  `int16(S − startS) ≥ 0`. Stop at epochs older than 2 s. `U = S − seqOff`.
- **SR translation**: `translateSR(layer, sr) (rtp uint32, ok)` succeeds only when `layer` is the current epoch's
  layer: `rtp = sr.rtp + tsOff`.
- Wraparound: all comparisons use int16 (seq) and int32 (ts) differences.

### 9.5 NACK and RTX (video and audio)

For each lost own seq `S` in a `TransportLayerNack` (each NACK item names up to 17 seqs):
1. `e, ok := m.lookup(S)`, then `U = S − e.seqOff` and `p := e.layer.cache.get(U)`. A miss is counted
   (`nack_missed`) and skipped.
2. Skip it if `S` was retransmitted in the last `max(20 ms, rtt/2)`, or 3 times already (ring of 1024 records keyed
   by `S`).
3. Retransmission budget: a token bucket of 25% of the DownTrack's forwarded bitrate (at least 200 kbps). Over budget,
   skip and count.
4. Build the packet: `ts = p.ts + e.tsOff`, marker from `p`. With RTX negotiated for the viewer PT: SSRC = `rtxSSRC`,
   PT = RTX PT, the DownTrack's own RTX seq counter, payload = `S` (2 bytes, big endian) + `p.payload`. Without RTX
   (always the case for audio): the original SSRC, PT and `S`.
5. Put it on `rtxQueue`, which the writer sends before new media. RTX payloads come from a `sync.Pool`.

The publish side needs nothing extra: Pion's NACK generator asks the publisher, and the recovered packet reaches the
cache and every forwarding DownTrack as a late packet.

### 9.6 Sender reports and A/V sync

- Publishers send SRs: Chrome from its capture-synchronized clock; native senders (M2) with NTP = wall clock at capture
  and RTP = that frame's timestamp, and no `report.SenderInterceptor` on their tracks (plan).
- When a DownTrack's writer handles an SR item for layer `L`, and `m.translateSR(L, sr)` succeeds, it sends
  `rtcp.SenderReport{SSRC: own, NTPTime: sr.ntp, RTPTime: sr.rtp + tsOff, PacketCount: own count, OctetCount: own count}`
  through `binding.pc.WriteRTCP`. The NTP value passes through unchanged. SRs are sent only while forwarding.
- Right after every new epoch, the writer sends a translated SR from the new layer's `lastSR` (if less than 10 s old),
  so the viewer re-syncs at once after a switch or when audio is turned on.
- Audio and video DownTracks of a share have the same msid stream (ShareID), so browsers lip-sync them with these SRs.
- Budget: the SFU adds no timestamp error while SRs are fresh. The in-process test asserts |A/V offset| ≤ 5 ms through
  switches and pauses. The 45 ms end-to-end target is measured in the browser e2e (05).

### 9.7 Keyframe requests

- `Share.requestKeyframe(slot)`: finds the current Layer for the slot. It sends only if `now − lastPLI ≥ 500 ms`,
  using a CAS on `lastPLI`, so concurrent callers produce one PLI:
  `pubPC.WriteRTCP(rtcp.PictureLossIndication{SenderSSRC: pubPC's RTCP SSRC, MediaSSRC: layer.ssrc})`. Throttled calls
  are counted.
- Callers: a DownTrack binding on a connected PC; the sub PC reaching `connected` (every video DownTrack); a target
  change; a viewer's PLI or FIR (translated to PLI upstream); and, while a DownTrack waits for a keyframe, every packet
  of the target layer. The last one re-requests every 500 ms until the keyframe arrives, which heals a lost PLI.
- There is no keyframe cache or replay: P-frames after an old keyframe don't decode.

### 9.8 Pacing (keyframe bursts)

- One token bucket per video DownTrack: rate = `max(3 × EWMA bitrate of the current layer, 2 Mbps)`, depth =
  `max(20 ms × rate, 16 KiB)`. A 150 KB 1080p keyframe then spreads over about 50 ms instead of hitting the socket in
  one burst for every viewer.
- Audio is never paced. It has its own goroutine, so video pacing never delays it.
- Pacing time spent and queue drops feed the allocator's congestion signal.

## 10. Layer selection and downlink adaptation (M1)

### 10.1 Requested quality → layer

| Requested | Layer choice | If none of those layers exists |
|---|---|---|
| `high` | `f`, else `h`, else `q` (a layer exists once its track arrived, even while paused, §11) | `off`, reason `no_layer` (share not live yet) |
| `low` | `q`, else `h` | `off`, reason `no_preview_layer`. A full layer is never sent as a thumbnail: a 25–40 Mbps 4K layer would multiply egress (the plan's "4K60 alone" case) |
| `off` | none | – |
| audio `true` | the audio Layer | `Audio: false` in the state event |

- Effective video quality = `min(requested, cap)`. The cap comes from the allocator (`bandwidth`), from server
  limits (`server_limit`) or from codec state (`codec_mismatch`, `decoder_unavailable`).
- Audio follows focus: the audio DownTrack is active only while its subscription has `Audio: true`. The client sends
  that for the focused share (05). The SFU doesn't force a single audio stream; a speaker button on another tile just
  sends another update.
- A change of target sets the interest mask to target|current, requests a keyframe, and keeps forwarding the old layer
  until the new one's keyframe arrives (S4: 42–106 ms). Pause is immediate.
- `SubscriptionStateEvent` is sent only when `Forwarded`, `Audio` or `Reason` changes, not on each request.

### 10.2 Downlink allocator (one per subscriber Conn, Galene-style)

Inputs: the latest REMB from the viewer (if any), loss EWMA from RRs of its video DownTracks, queue drops and pacing
delay, and the measured bitrate of each target layer.

`required(high)` = Σ audio-on bitrates + Σ bitrate of every target layer when every request is honoured. Allocation
order, cheapest first:
1. audio;
2. every `low` (thumbnail): always kept, never turned off by the server. The client decides how many thumbnails a phone
   shows;
3. `high` requests, the most recently requested first. Each is served at `f` if the budget allows, else at `q`.

Budget = `0.9 × REMB`, or unlimited when no REMB has been seen for 5 s and loss is low.

| State | Condition | Action |
|---|---|---|
| `normal` | REMB < 0.85 × required for 2 consecutive 1 s ticks, **or** loss ≥ 10% averaged over 2 s, **or** more than 50 queue drops in 1 s | cap the `high` requests that don't fit (oldest first) at low, reason `bandwidth` → `limited` (backoff = 10 s) |
| `limited` | backoff elapsed and loss < 2% over the last 3 s | remove the caps (trial upgrade) → `probing` |
| `probing` | within 6 s: REMB < 0.8 × new required or loss ≥ 5% | cap again, backoff = min(2 × backoff, 120 s) → `limited` |
| `probing` | 6 s pass cleanly | backoff = 10 s → `normal` |

Why trial upgrades: without TWCC probing, Chrome's REMB stays near 1.5× what it currently receives, so waiting for
REMB to exceed the high layer's bitrate would never end. The price is a short quality dip at most every 2 minutes on a
constrained link. TWCC downlink in v2 replaces this; the allocator's interface stays.

*Later (M5)*: `server_limit`, a server-wide egress soft cap: while total egress (2 s EWMA) is above it, new or
upgraded `high` requests are capped at low. M1 has no such setting (03 stores none); the reason value is reserved.

## 11. Publisher-side control (uplink)

```go
type UplinkPolicy interface { // one per Share, called from the ticker every 1 s
	Tick(s ShareUplinkStats) UplinkDecision
}
type ShareUplinkStats struct {
	Client ClientKind
	Layers []LayerUplinkStats // RID, bitrate, loss fraction, jitter, number of DownTracks forwarding or waiting for it
}
type UplinkDecision struct {
	MaxBitrate int             // bps; 0 = none
	Active     map[string]bool // per rid; nil = no change
}
```

- **M1, every publisher**: `MaxBitrate` = `Limits.MaxShareKbps` (03 setting `maxShareBitrateKbps`, pinnable by 04's
  `limits.max_bitrate_kbps`; 0 = none). When it is set, the SFU sends REMB
  `{bitrate, SSRCs of the video layers}` on the pub PC every 1 s and a `QualityHintEvent` when the value changes.
  Chrome applies REMB as an upper bound under its own GCC. The web sharer also applies `maxBitrate` through
  `setParameters` (05).
- **M1 stretch, layer pausing** (04 config key `sfu.pause_unwatched_layers`, default on; cuttable to M5 without a
  protocol change): when no DownTrack has
  wanted `f` for 10 s, send `QualityHintEvent{Layers: [{f, false}]}`. As soon as one does, send `{f, true}` and a PLI.
  Browsers set `encodings[f].active` (05). This saves the sharer about 8 Mbps of upload while nobody focuses the
  share, at the cost of 0.2–0.5 s more on the first focus. `q` is never paused while anyone is subscribed.
- **Later (M2), native publishers**: a loss and receive-rate estimator per share (Galene's estimator design). It sends
  REMB plus `QualityHintEvent{MaxBitrate}` with the plan's hysteresis (changes of at least 15%, at most every 2 s).
  Browsers keep their own GCC over TWCC.

## 12. Limits and guards (M1)

Guards limit abuse, not people. Every number here is a constant in `config.go`, except the soft limits.

| Guard | Value | On violation |
|---|---|---|
| PCs per Conn | 1 pub + 1 sub | `sfu.pc_limit` |
| PC creations per Conn | 10 per minute | `sfu.pc_rate_limited` (retryable) |
| ICE + DTLS handshake | 10 s to `connected` for a new PC; DTLS context 10 s; ICE-TCP first STUN 10 s | close PC, `PCStateEvent{failed, handshake_timeout}` |
| PC after `failed` | 30 s grace | close; pub shares stay `stalled` (the hub's 30 s timeout ends them) |
| Shares | 4 per participant (defense in depth; the hub enforces 4 per user and the room soft limit, 01) | `sfu.too_many_shares` |
| Publish offer | ≤64 KiB, ≤8 m-lines, ≤3 rids from {f,h,q}, no data channel | §6.3 codes |
| Buffered remote candidates | 32 per PC before its remote description; 64 per PC in total | extra ones dropped, logged at debug |
| Subscriptions | 256 per Conn; 64 items per `UpdateSubscriptions` | `sfu.too_many_subscriptions` |
| Actor command queue | 64; 5 s wait | `sfu.busy` |
| Share without media | none here: the hub ends shares `starting` or `stalled` for 30 s (`media_timeout`, 01 §4.4) | – |
| Keyframe requests upstream | 1 per 500 ms per layer | throttled (counted) |
| NACK | ≤1 s old, ≤3 retransmits per seq, 25% budget | skipped (counted) |
| DownTrack queues | video 1024, audio 256, RTX 256 | drop newest (counted) |
| Probe PCs | 3 per user, 20 total, 20 s lifetime | `sfu.probe_limit` |
| UDP buffers | 8 MiB requested by 04's netx | warning in doctor (04) |

Admin soft limits (0 = unlimited, the default; editable in the admin UI and stored by 03; pushed live with
`SetLimits` by the wiring from `SettingsCache.OnChange`, 04 §6.6). The room share limit and the participant limit
are enforced by the hub (01), not here.

```go
type Limits struct {
	MaxShareKbps int // cap per share: REMB, hint (§11) and ShareParams (§8.6); 03 maxShareBitrateKbps
}
// later: type RoomLimits struct{ MaxShares, MaxShareKbps int } and a server-wide egress cap (§10.2)
```

Config keys: the ICE ports, loopback candidates and socket buffers are 04's keys (`listen.ice_udp`,
`listen.ice_tcp`, `network.include_loopback`, `network.udp_buffer_bytes`, `network.exclude_interfaces`; 04 §4.3) and
reach the SFU inside `Config.Transport`. The only SFU key, registered in 04's `config/keys.go`:

```toml
[sfu]
pause_unwatched_layers = true   # §11 layer pausing (env ISSHONI_SFU_PAUSE_UNWATCHED_LAYERS)
```

```go
type Config struct {
	Transport            *netx.Transport // 04: sockets, muxes, filters, rewrite rules (built and closed by 04)
	PauseUnwatchedLayers bool
	Limits               Limits
}
// Tests inject sockets (e.g. sfutest.FaultConn) through 04's netx.TransportOptions.PacketConns.
```

## 13. Observability (M1)

- `(*SFU).Snapshot() Snapshot`: rooms, then per share `ShareInfo` plus ingress per layer (bitrate, loss, fps), egress
  bps, subscriber counts by forwarded quality, audio-on count. Plus totals: ingress and egress bps, DownTracks, PCs by
  state, selected transports (udp / tcp443 / tcp7882). This feeds the admin dashboard (04, 05): "bitrate, layer, loss
  and egress".
- `(*Conn).Stats() ConnStats`: per PC (state, selected transport, RTT), per own share (layers, bitrates, loss), per
  subscription (requested, forwarded, reason, layer, bitrate, NACKs served, drops, profile), and the downlink
  estimate. Signal sends it as `stats` (01) every 2 s while the client asks for it.
- `(*SFU).Metrics() Metrics`: plain atomics, cheap to read. 04 maps them to Prometheus (optional, off by default):

| Metric | Type | Labels |
|---|---|---|
| `isshoni_sfu_conns` | gauge | `role` |
| `isshoni_sfu_peerconnections` | gauge | `kind` (pub, sub), `state` |
| `isshoni_sfu_selected_transport` | gauge | `transport` (udp, tcp443, tcp7882) |
| `isshoni_sfu_shares` | gauge | `state` |
| `isshoni_sfu_downtracks` | gauge | `kind`, `forwarding` (high, low, off) |
| `isshoni_sfu_ingress_bytes_total`, `isshoni_sfu_egress_bytes_total` | counter | `kind` |
| `isshoni_sfu_ingress_packets_total`, `isshoni_sfu_egress_packets_total` | counter | `kind` |
| `isshoni_sfu_nack_received_total`, `_nack_served_total`, `_nack_missed_total`, `_rtx_skipped_total` | counter | |
| `isshoni_sfu_pli_sent_total`, `_pli_throttled_total`, `_keyframes_received_total` | counter | |
| `isshoni_sfu_layer_switches_total` | counter | |
| `isshoni_sfu_downgrades_total` | counter | `reason` |
| `isshoni_sfu_queue_drops_total` | counter | `kind` |
| `isshoni_sfu_handshake_timeouts_total` | counter | `kind` |
| `isshoni_sfu_packet_cache_bytes` | gauge | |

  No per-share or per-user labels (unbounded cardinality). Per-share data is in `Snapshot`.
  `Metrics.EgressBytes` is the cumulative counter 04 uses for month-to-date transfer.
- **Logs** (slog, `component=sfu`): Conn join and leave (IDs only), PC state changes with the selected transport type,
  share lifecycle, codec-policy changes, downgrades (info); handshake timeouts, small buffers, negotiation timeouts
  (warn). Never SDP, never ICE candidates or client IPs above debug level, never tokens.

## 14. Code reuse and attribution

Reimplement from the designs by default. When a function is copied or closely translated, keep the upstream copyright
line in that file with "Modified for isshoni", and add an entry to `NOTICE` or `THIRD_PARTY_NOTICES` (06 generates
and checks them).

| Source | License | What to borrow (design, or code with attribution) |
|---|---|---|
| Galene (`github.com/jech/galene`) | MIT (copy the license text into THIRD_PARTY_NOTICES) | `packetcache` (ring with seq validation), `estimator` (rate and packet estimator), REMB/loss-driven layer choice in `rtpconn`, NACK recovery budget |
| LiveKit server (`github.com/livekit/livekit-server`, `pkg/sfu`) | Apache-2.0 (keep file headers, carry their NOTICE lines if the repo has a NOTICE, state changes) | `rtpmunger` (offsets and the padding drop rule), `forwarder` (target/current layer and reference-timestamp switching), `sequencer` (own seq → upstream metadata; M1 uses the simpler epoch ring), `downtrack` SR handling, `pacer` |
| Pion (`interceptor`, `rtp`) | MIT | used as dependencies. `nack` send-buffer and `report` code as reference only |
| isshoni spikes S4 and S2-VT | Apache-2.0 (ours) | munger, `isH264KeyframeStart`, `h264FmtpKey`, tests; the SPS parser |

No code from GPL projects (Screego, OBS) and none from AGPL.

## 15. Fake media and the load test (M1)

### 15.1 `internal/media/fake`: decision

| Option | Verdict |
|---|---|
| Pre-encoded H.264 files generated at build time | Rejected. CI would need an encoder binary: x264 is GPL (banned), and FFmpeg or OpenH264 adds a native toolchain and download step. Fixed resolutions and several MB of blobs per profile in git |
| A real encoder in Go | None exists. cgo encoders break `CGO_ENABLED=0` |
| **Chosen: synthetic codec-shaped H.264 for load, plus a tiny pure-Go I_PCM/P_Skip encoder when frames must decode** | The SFU reads only NAL headers and SPS, so synthetic streams give accurate packet rates, sizes and keyframe bursts at almost no CPU. The load generator then measures the SFU, not an encoder. The decodable mode (about 400 lines, written from the H.264 spec) is deterministic, any resolution, and license-clean |
| Opus | A committed 1 s asset of 50 packets (about 8 KB), generated once with `opusenc` (opus-tools and libopus, BSD) by `testdata/gen-opus.sh` plus a pure-Go Ogg page reader. CI never needs opus-tools. There is no pure-Go Opus encoder |

`internal/media/fake` is test code: `cmd/isshoni` never imports it and no release artifact contains it (06's build
checks that).

```go
package fake

type Kind uint8 // Video | Audio
type Packet struct { // mirrors the C ABI packet {kind, layer, keyframe, capture_ts_ns, annexb|opus, reinit}
	Kind      Kind
	Layer     string // "f" | "q" | "" (audio)
	Keyframe  bool
	CaptureNS int64  // monotonic capture time
	Data      []byte // Annex B access unit (with emulation prevention) or one Opus packet
	Flash     bool   // this frame shows the sync flash
	Beep      bool   // this audio packet starts the sync beep
}
type Mode uint8 // Synthetic | Decodable
type VideoLayer struct {
	RID                string
	Width, Height, FPS int
	Bitrate            int // bps (Synthetic)
}
type Config struct {
	Mode       Mode
	Profile    string        // Synthetic: "6400" or "42e0" (SPS profile, to exercise the codec policy). Decodable: always "42e0"
	Layers     []VideoLayer  // default: f 1920x1080@60 8 Mbps; q 640x360@15 0.3 Mbps
	GOP        time.Duration // default 3 s
	Audio      bool
	FlashEvery time.Duration // default 1 s: one flash frame on every layer and one beep at the same capture instant
	Seed       uint64
}
func New(cfg Config) (*Source, error)
func (s *Source) Next(ctx context.Context) (Packet, error) // pull, like im_next_packet(timeout)
func (s *Source) RequestKeyframe(layer string)
func (s *Source) SetBitrate(layer string, bps int)
func (s *Source) Close() error
```

- **Synthetic AU**: a keyframe is a real SPS (chosen profile, level, size) + PPS + an IDR slice NAL. A delta frame is
  one non-IDR slice NAL. After the NAL header byte, the slice payload carries an 18-byte marker (magic `ISHN`, rid,
  flags {keyframe, flash}, frame index uint32, capture ns uint64), then seeded filler bytes, all with standard
  emulation prevention. Frame sizes: `P = n·avg/(n − 1 + 8)` and `K = 8·P` over a GOP of `n` frames, with ±25% jitter
  on P, so each layer averages its bitrate. `SetBitrate` changes `avg` at once.
- **Decodable AU** (Constrained Baseline, CAVLC, `pic_order_cnt_type = 2`, one slice per frame, deblocking off with
  `disable_deblocking_filter_idc = 1`, width and height padded to multiples of 16 with SPS cropping):
  - IDR: every macroblock is I_PCM (samples clamped to 16–235).
  - P frames: P_Skip everywhere (every motion vector predicts to zero) except the changed macroblocks, which are I_PCM
    (mb_type 30): a moving 32×32 box, a frame counter drawn as blocks, and a 64×64 flash square in the corner on flash
    frames.
  - Default 640×360@30 for `f` and 320×180@15 for `q`. About 4 Mbps for `f` (dominated by the I_PCM IDR). Browsers
    decode it everywhere.
- **Opus**: the asset loops (a 440 Hz bed with whole cycles per second, so the loop is phase-continuous, plus a 1 kHz
  beep in the first 100 ms of each second). The beep packet is packet 0, in sync with the flash frame.

`internal/client/publish` (M1 subset): `Publisher{PC, Source}` creates one video TrackLocal per rid, stamping the
mid/rid extensions itself (Pion's built-in tracks don't for simulcast senders, as S4 found), plus one audio track. It
packetizes with `pion/rtp/codecs` (`H264Payloader`, MTU 1200; Opus as is), computes `rtp_ts = base + (capture −
t0) × clock`, and sends its own SRs every 1 s per SSRC (NTP = wall clock at capture, RTP = that frame's timestamp,
exactly the plan's native path). It answers PLI with `Source.RequestKeyframe` and registers the TWCC header-extension
interceptor, so the SFU's feedback generator does real work. No rate control in M1.

`sfutest.Viewer`: a Pion subscriber (NACK generator, RR, optional REMB injection) that records per track: seq, ts,
arrival, marker contents (layer, frame, flash), gaps, recovered-by-RTX, final loss after 1 s, keyframes, SRs. It offers
the S4 checks (`CheckContinuous`, `CheckStartsOnSPS`) plus `AVOffset()` (flash vs beep, both mapped to NTP through the
last forwarded SRs).

### 15.2 `cmd/isshoni-loadtest`

```
isshoni-loadtest -server https://example.org -admin admin   # password from ISSHONI_LOADTEST_PASSWORD or a prompt
  [-scenario focus|all-high|smoke]  [-publishers 10] [-subscribers 10] [-duration 10m] [-warmup 60s]
  [-full 1920x1080@60:8M] [-preview 640x360@15:300k] [-gop 3s] [-profile 6400|42e0] [-decodable]
  [-focus-rotate 30s] [-login-interval 3s] [-json out.json] [-keep-users]
```

- **Setup**: log in as admin, create an invite with `publishers + subscribers` uses (`POST /api/v1/invites`, 03),
  register users `lt-<run>-<n>` with random passwords (`POST /api/v1/auth/register` returns the session cookie; one
  request per `-login-interval` to stay under 03's `auth-ip` bucket), open WebSockets, join the default room.
  **Teardown** deletes those users (`DELETE /api/v1/admin/users/{id}`) unless `-keep-users`.
- Each simulated client is in-process: a signaling client (01's `internal/client/signal`), and Pion PCs with the same
  interceptors as a browser (NACK, RR, TWCC header extension on publish).
- **Publishers**: `fake.Source` (Synthetic by default) → `publish.Publisher`, preset Auto, audio on.
- **Scenario `focus`** (the plan's pass test): subscriber `i` treats publisher `i` as itself. It focuses publisher
  `(i+1) mod N` (`high` + audio) and shows the other 8 as thumbnails (`low`). Every `-focus-rotate`, at a random offset
  per subscriber, focus moves to the next share. Expected egress ≈ N × (8 + 8 × 0.3 + 0.128) Mbps ≈ **105 Mbps** for
  10×10.
- **Scenario `all-high`** (stress, report only): every 30 s, 10 more `low` subscriptions become `high`, until loss
  exceeds 2% or server CPU exceeds 90%. Reports egress, CPU and loss at each step (the knee). The plan's full
  all-high is about 730 Mbps.
- **Scenario `smoke`** (CI): 2×2, 30 s, `-decodable`, the same checks with relaxed thresholds.
- **Server data**: every 5 s, `GET /api/v1/admin/dashboard` (04 §11.4: `server.process` with CPU seconds, RSS and
  NumCPU, the live rooms and shares from `Snapshot`, and totals). Once before clients join (baseline RSS) and during
  the run.
- **Client data**: per track from `sfutest.Viewer`; per rotation, the switch latency (update sent → first packet of
  the new layer); A/V offset per focused share.

Pass criteria for `focus` (after warm-up; exit 0 = pass, 1 = fail, 2 = setup error):

| Check | Pass |
|---|---|
| Server CPU | process CPU ÷ (wall × NumCPU) < 50% on average (4 vCPU VPS) and < 65% in any 5 s sample |
| Loss | unrecovered loss after NACK/RTX < 0.5% of expected packets, over all subscriber tracks (pre-recovery loss is reported too) |
| PLI storms | upstream PLIs per layer ≤ 0.2/s on average, and never more than 10 in any 10 s window |
| Memory | (RSS − baseline RSS) ÷ DownTracks < 1 MB |
| Egress | within ±15% of the expected value (sanity check of the scenario) |
| Switching | p95 switch latency < 1 s; every switch starts on an SPS; seq continuous per track |
| A/V | p99 \|flash − beep\| ≤ 5 ms at the SFU level |

Output: a human summary plus JSON: `{"scenario","params","server":{"cpu":{avg,p95},"rssPerDownTrack","egressMbps"},
"loss":{"raw","final"},"pli":{…},"switchMs":{p50,p95,max},"avOffsetMs":{p50,p99},"pass":true,"failures":[]}`.

## 16. Room for later milestones

| Later | What M1 already provides |
|---|---|
| M2 Windows app / M3 macOS app (native publishers, `RolePublisher`) | Roles; `tracks`-based share mapping; forwarded publisher SRs; `UplinkPolicy` slot; `QualityHintEvent.MaxBitrate`; REMB to publishers; High `64001f`/`640c1f` in the MediaEngine, matched without level; codec-policy re-offer path; `internal/client/publish` and `internal/media/fake` shaped like the C ABI |
| M3: 4K60 without a preview layer | `low` → `off` with `no_preview_layer`; single-layer shares |
| M4 Linux agent (`RoleAgent`) | publish-only role |
| M5 720p30 middle layer | rid `h` accepted and resolved; add `QualityMedium` (additive on the wire) |
| M5 BWE improvements, v2 TWCC downlink | allocator interface; the sub API adds transport-cc and a GCC-style estimator; probing replaces trial upgrades |
| Egress above ~50k pps | `sendmmsg` via pion/transport batchconn behind the mux |
| VP8/VP9/AV1/HEVC | codec-keyed munger hooks (picture ID and TL0PICIDX rewriting for VP8); `ProfileKey` generalizes to a codec key |
| Optional E2EE (LiveKit-style frames) | keyframe detection must read the unencrypted NAL header bytes that format keeps |
| Embedded TURN | SettingEngine relay settings only; the model doesn't change |
| Mobile apps (`ClientMobile`) | no SFU change |

## 17. Test plan

All Go tests run with `-race`. `TestMain` runs `goleak.VerifyTestMain` (Pion goroutines allowed 2 s to settle after
`Close`). Timers (allocator, codec policy, share and PC timeouts) use an internal clock interface that tests replace
through `export_test.go`; integration tests use real time unless noted.

**Unit (M1)**
- `codec_test`: `ProfileKey` for fmtp variants (case, missing, `640034`); the compatibility matrix table; `bestPT` exact
  before compatible; golden SDP fragments from both MediaEngines (PTs, fmtp, feedback, extensions; no transport-cc on
  sub).
- `h264_test`: the S4 keyframe cases plus malformed STAP-A; SPS size parsing with SPS units from S2-VT and a Chrome
  capture; `FuzzKeyframeStart`, `FuzzParseSPS` (no panics, bounded work).
- `munger_test`: all S4 tests ported, plus: seq and ts wraparound; epoch lookup for NACK across 3 switches; padding
  gap closing (in-order) and out-of-order padding ignored; late packets across a padding epoch mapped and across a
  layer switch dropped; SR-aligned ts on switch (exact value) with fallback when an SR is missing or stale; the
  monotonic guard; resume on the same Layer keeps `tsOff`; a new Layer instance with the same rid is a new epoch;
  profile change waits for a keyframe; `FuzzMunger` (random interleavings keep the output seq continuous and ts
  monotonic).
- `packetcache_test`: sizing (pps → capacity, clamps, grow and shrink hysteresis); out-of-order insert; wraparound;
  1.5 s expiry; concurrent insert/get under `-race`; benchmark insert+get < 100 ns.
- `throttle_test`: 100 concurrent `requestKeyframe` → 1 PLI; another after 500 ms, not before.
- `rtx_test`: RTX packet layout (OSN, SSRC, PT); per-seq limits; budget.
- `allocator_test` (fake clock): scripted REMB and RR sequences → state changes, backoff doubling and reset, events
  once per change, allocation order (audio, thumbnails, newest high).
- `pacer_test`: a 150 KB burst at 24 Mbps takes 50 ms ± 10%.
- `sdpcheck_test`: each validation code; Opus `maxaveragebitrate` edit per m-line; the answer check for m-lines
  without H.264.
- `policy_test` (fake clock): high → cb at once; cb → high after 60 s; conns without H.264 ignored.
- `api_test`: with a loopback `netx.Transport` (04) that has a fake 443 ICE listener, a sub offer contains UDP 7882,
  passive TCP 443 and 7882 candidates and no trickle; the probe APIs each advertise only their transport; the
  remote-candidate filter drops loopback, link-local, private (unless the server's own address is private) and
  multicast candidates. IP selection, rewrite rules and buffer read-back are 04's `netx` tests.
- `fake` tests: synthetic bitrate within ±5% over 30 s; markers survive emulation prevention; the decodable SPS/PPS and
  slice headers parse with the SFU's parser; macroblock count and I_PCM alignment checked by a small bit reader.

**In-process integration (M1, `sfutest.Harness`: real `sfu.SFU` on loopback, `DirectSignaler`, `publish.Publisher`
with `fake.Source`, `sfutest.Viewer`)**
1. `TestBasicForwarding`: the first video packet a viewer receives is an SPS (DTLS gate); audio flows; `ShareUpdated`
   goes pending → live.
2. `TestLayerSwitchOnKeyframe` (S4 port): high → low → off → high; switch < 1 s; continuity; every switch starts on an
   SPS; nothing flows while `off`.
3. `TestAudioFollowsFocus`: audio only for `Audio: true`; toggling works without renegotiation; seq continuous across
   the pause.
4. `TestNACKFromCache`: `FaultConn` drops chosen outgoing packets to one viewer. The viewer's NACKs are served from
   the cache (video as RTX, audio plain), with payload bytes equal to the originals; the cache's miss counter stays 0.
5. `TestSRTranslationAndAVOffset`: |flash − beep| ≤ 5 ms after 10 layer switches and 5 audio off/on cycles; forwarded
   SR RTP equals upstream RTP + offset.
6. `TestPaddingNoGaps`: the publisher injects padding-only packets; the viewer sees no seq gaps and sends no NACKs.
7. `TestSubICERestart`: black-hole the viewer's UDP for 5 s; the viewer calls `RestartICE`; media resumes within 3 s
   of unblocking on the same SSRC; the share never leaves `live`.
8. `TestPubPCRebuild`: the publisher replaces its pub PC and offers `gen + 1` with the same share ids in `tracks`;
   viewers get no new sub offer; video resumes on a keyframe; the share goes `stalled` → `live`; ShareID unchanged.
9. `TestSubPCRebuild`: `ResetPC(sub)` → new offer with all subscriptions; media resumes.
10. `TestConnCloseEndsShares`: `ShareEnded(conn_closed)`; viewers' m-lines go inactive; the next share reuses them
    (the m-line count in the SDP doesn't grow over 5 share cycles).
11. `TestResync`: drop the offer on purpose, call `Resync()`: the same offer (same `gen` and `neg`) is sent again; the answer is
    applied.
12. `TestCodecPolicy`: a CB-only viewer joins while the publisher sends `6400` → `CodecPolicyEvent{42e0}` →
    re-offer → the answer holds only `42e0`/`4200` → the viewer receives video on a CB PT; policy returns to `high`
    60 s after it leaves (fake clock hook).
13. `TestDecoderUnavailable`: a viewer MediaEngine without H.264 (fresh Firefox) → audio only, `decoder_unavailable`;
    `SetDecodeCaps` with CB → sub PC rebuilt → video flows. The guard path: caps claim H.264 but the answer rejects
    it → no negotiation failure, then a retry.
14. `TestHandshakeTimeout`: a client that never completes ICE → PC closed at 10 s ± 1 s, `PCStateEvent`.
15. `TestGuards`: a third PC, a fifth share, a bad rid, an unknown track binding, no H.264, a data channel → the §6.3
    codes, with nothing applied.
16. `TestDownlinkLimited`: the viewer injects REMB 1 Mbps → focused share forwarded `low` with reason `bandwidth`;
    REMB 20 Mbps and zero loss → trial upgrade → `high`.
17. `TestTenByTen` (skipped with `-short`, S4 port): 10 publishers × 10 viewers; all 100 subscriptions on the right
    layer within 5 s; audio only on focused shares; integrity checks; heap growth per DownTrack < 1 MB.
18. `TestProbe`: a Pion data-channel offer per transport (`udp`, `tcp443`, `tcp7882`) → connected; the answer holds
    only that transport's candidates; data-channel messages come back unchanged; a 4th concurrent probe of one user →
    `sfu.probe_limit`; `tcp443` without a 443 mux → `sfu.transport_disabled`.

**Browser e2e (owned by 05, relies on this doc)**: Chrome sharer (canvas) → SFU → Chrome and Firefox viewers:
`framesDecoded > 0`, audio energy, switches, and the codec policy with Firefox; the Go decodable publisher → the same
browsers. The 45 ms A/V target is measured there.

**Load (M1)**: `isshoni-loadtest -scenario focus` on a 4 vCPU VPS from a second VM, pass criteria in §15.2 (a manual
milestone gate before the M1 exit test). `-scenario smoke` runs in CI against a server started by the job (06).

## 18. Interfaces other docs rely on

- **Package `internal/server/sfu`** (§6.1–6.3): `New`, `Config` (with `Transport *netx.Transport`), `Deps`, `Limits`,
  `SFU` (`Close`, `Ready`, `Join`, `Shares`, `Share`, `CodecPolicy`, `StopShare`, `CloseRoom`, `SetLimits`,
  `Snapshot`, `Metrics`, `Probe`), `Conn` (every method in §6.1), `JoinParams`, `StartShareParams`, `ShareParams`,
  `EncodingParams`, `TrackBinding`, `ShareUpdate`, `SubscriptionUpdate`, `DecodeCaps`, `ProfileKey`, `Role`,
  `ClientKind`, `PCKind`, `Quality`, `Preset`, `SourceKind`, `ShareState`, `EndReason`, `SubReason`, `ShareInfo`,
  `LayerInfo`, `Signaler`, `RoomEvents`, `Event` and its types (`SubscriptionStateEvent`, `CodecPolicyEvent`,
  `QualityHintEvent`, `LayerHint`, `PCStateEvent`, `ErrorEvent`), `Error` and the `sfu.*` codes, `ConnStats`,
  `Snapshot`, `Metrics`, `ProbeTransport`, `ProbeResult`. Later: `RoomLimits`, `SetRoomLimits`.
- **Wire-visible conventions** (through 01's `sfuplane`): rids `f`/`q` (`h` reserved); share ids from the hub; sub
  msid = share id and track ids `v-`/`a-`+share id (debugging aid; mapping is by `tracks`); transceivers are reused
  (a `track` event fires again on reuse); the server sends complete SDP and never trickles; `gen`/`neg` on offers and
  answers; the publish answer's Opus `maxaveragebitrate` follows the preset; the sub offer's Opus has `stereo=1` (the
  viewer must add `stereo=1` to its answer).
- **Static PT table** (§8.1). **Codec policy** semantics (§8.3). **Encodings per preset** (§8.6).
- **Config key** `sfu.pause_unwatched_layers` (registered in 04's registry).
- **Metric names** (§13).
- **`internal/media/fake`** (`Source`, `Packet`, `Config`, `Mode`) for M2's fake engine; **`internal/client/publish`**
  (`Publisher`) as the start of M2's client core; **`internal/server/sfu/sfutest`** (`Harness`, `DirectSignaler`,
  `Viewer`, `FaultConn`) for 01's integration tests in `internal/server/itest`.
- **`cmd/isshoni-loadtest`** CLI and exit codes (§15.2) for 06's CI smoke job and the exit-test pre-flight.

## 19. Depends on

**01-protocol.md**
- The wire format and `internal/server/sfuplane` (01 §15.4), which calls this package exactly as §6.4 shows: share
  ids in `StartShareParams.ID`, `gen`/`neg` and `tracks` on offers and answers, `pc.restart` → `RestartICE`/`ResetPC`,
  `pc.close` → `ClosePC`, caps as `h264/<4 hex>` CodecKeys (converted to `ProfileKey`).
- Session semantics: signal keeps one `Signaler` per Connection across WebSocket resumes, calls `Resync()` on resume
  and `Close(disconnected)` after 30 s; the hub owns share ids, share limits, the share lifecycle and its 30 s
  timeouts, and always calls the publishing Conn to stop a share.
- A Go signaling client (`internal/client/signal`, 01 §15.3) that the load test imports.

**03-accounts-and-store.md**
- User and room IDs as strings (12-char lowercase base32; the default room is `lounge`).
- The setting `maxShareBitrateKbps` (pushed through `SetLimits` by the wiring).
- For the load test: admin login (`POST /api/v1/auth/login`), `POST /api/v1/invites` with `maxUses` up to 1000,
  `POST /api/v1/auth/register` with the invite token (it returns the session cookie, so no second login), and
  `DELETE /api/v1/admin/users/{id}`. Invite registrations don't consume 03's `register-ip` bucket; the load test still
  paces registrations at `-login-interval` (default 3 s) for the `auth-ip` bucket (burst 20, 1 per 15 s).
- Room deletion reaches the SFU through the hub (`Conn.Close`), not directly.

**04-server-platform.md**
- `netx.Transport` (04 §7.3) with `UDPMux`, `TCPMux` (combined), `TCPMux443` (nil in `tls.mode=off`), `TCPMux7882`,
  `Apply(*webrtc.SettingEngine)` and `TransportOptions.PacketConns` for tests.
- The wiring (04 §6.6): builds the SFU after the Transport, registers 01's `sfuplane` as `Deps.Events`, calls
  `SetLimits` on settings changes, `/readyz` uses `SFU.Ready()`, a Prometheus adapter over `Metrics()`, month-to-date
  transfer from 04's socket counters (not from the SFU), and shutdown order: hub first, then `SFU.Close()`.
- `POST /api/v1/conntest` (04 §7.7) on top of `SFU.Probe`.
- The admin dashboard `GET /api/v1/admin/dashboard` with `server.process` (CPU seconds, RSS, NumCPU) plus the live
  rooms built from `Snapshot()` (the load test polls it every 5 s).

**05-web-client.md**
- The sharer sends simulcast with rids `f` and `q` using `ShareParams.encodings`, lists its m-sections in `tracks`,
  orders `setCodecPreferences` by `ShareParams.codec` / `quality.hint.codec` (High first when allowed, S4 finding 4)
  and re-offers on change, and applies `quality.hint` encodings (`active`, `maxBitrate`).
- The viewer maps tracks by `tracks`; adds `stereo=1` to its sub answer; sends decode caps and polls them while
  H.264 is missing (no client codec retries); sends `pc.restart{mode: ice}` after 3 s of ICE `disconnected` and
  `pc.restart{mode: rebuild}` on `failed`; shows the four wire status reasons.
- The connection-test UI calls 04's endpoint. The e2e covers the compatible cells of §8.2 and the 45 ms A/V target.

**06-deploy-and-ci.md**
- sysctls `net.core.rmem_max` and `wmem_max` = 8388608 in install.sh and the Docker docs; compose publishes
  `7882/udp`, `7882/tcp` and `443` with the same host and container ports.
- CI runs `go test -race ./...` (the 10×10 test outside `-short`) and the load-test `smoke` scenario; release builds
  exclude `internal/media/fake`, `sfutest` and `isshoni-loadtest`; NOTICE and THIRD_PARTY_NOTICES entries for
  Galene and LiveKit if code is adapted.

## 20. Implementation slices (in order; each is testable on its own)

The integrated plan (`README.md`, slice ids `Sxx`) sequences these with the other docs; the numbers here are this
doc's.

| # | Slice | Scope | Acceptance |
|---|---|---|---|
| 1 | Codec and H.264 helpers | `codec.go`, `h264.go`: `ProfileKey`, matrix, `bestPT`, PT table, both MediaEngine builders, keyframe detection, SPS parser, `sdpcheck.go` | `codec_test`, `h264_test`, `sdpcheck_test` pass; fuzzers run 60 s with no failure; golden SDP fragments match §8.1 |
| 2 | Munger and epochs | `munger.go` | `munger_test` incl. S4 ports pass; `FuzzMunger` 60 s clean; ≥90% statement coverage |
| 3 | Packet cache | `packetcache.go` | `packetcache_test` passes under `-race`; benchmark < 100 ns per insert+get |
| 4 | Fake media, publisher, viewer | `internal/media/fake` (Synthetic), `internal/client/publish`, `sfutest.Viewer` | Pion-to-Pion loopback test (no SFU): viewer sees the configured bitrate ±5%, markers intact, SRs sent every 1 s, PLI → keyframe within one frame |
| 5 | Transport integration | `api.go`: pub/sub/probe `SettingEngine`s on 04's `netx.Transport`, remote-candidate filter (needs 04's netx slice) | `api_test`: candidates (UDP 7882, TCP 443 via a fake listener, TCP 7882), no trickle, filter table |
| 6 | Core model, one layer | SFU, Room, Participant, Conn actor, pub/sub PCs with `gen`/`neg` and `tracks`, Share (media states), Layer, DownTrack (single layer, no RTX), `Signaler`, `RoomEvents`, `sfutest.Harness`, DTLS gate, PLI on connect, `Close` | Integration 1 passes; goleak clean; `Close` leaves no goroutines |
| 7 | Simulcast and selection | rids, `UpdateSubscriptions`, interest mask, keyframe switching, PLI throttle, audio follows focus, transceiver reuse, `SubscriptionStateEvent`, `ShareParams` per preset (§8.6) | Integration 2, 3, 10, 17; `throttle_test` |
| 8 | Repair and sync | NACK/RTX from cache, SR forwarding and translation, abs-send-time, padding | Integration 4, 5, 6; `rtx_test` |
| 9 | Resilience and guards | media states (`stalled`), pub rebuild by `gen`, sub rebuild, ICE restart, `Resync`, negotiation timeouts, handshake timeout, PC grace, `ClosePC`, all §12 guards | Integration 7, 8, 9, 11, 14, 15 |
| 10 | Codec policy and Firefox | room policy with hysteresis, answer filtering (verify Pion honours preferences, else fallback), Opus preset edit, caps-driven video, sub rebuild, `Bind` guard with 20 s × 9 retries | Integration 12, 13; `policy_test`, `sdpcheck_test` |
| 11 | Downlink adaptation | allocator, pacer (`server_limit` is later) | `allocator_test`, `pacer_test`, integration 16 |
| 12 | Observability | `Snapshot`, `ConnStats`, `Metrics`, logs | a test reads every metric after integration 2 with non-zero counters where expected; no SDP or IPs in info logs (log capture test) |
| 13 | Uplink control | `UplinkPolicy`, admin cap REMB and hint, `SetLimits`, layer pausing (stretch; cuttable to M5 without a protocol change) | a test with `MaxShareKbps` → REMB on the pub PC every 1 s; layer pausing hint after 10 s unwatched and resume with PLI |
| 14 | Connection-test probe | `probe.go`, probe APIs, echo channel | Integration 18; limits enforced |
| 15 | Decodable fake and load test | decodable H.264 and the Opus asset; `cmd/isshoni-loadtest` (setup through 03's REST, scenarios, measurements, JSON, exit codes) on 01's `internal/client/signal` | `fake` tests; `smoke` passes against a local server; a Chrome e2e (05) decodes the decodable stream |
| 16 | Load gate | run `focus` on a 4 vCPU VPS from a second VM; fix what fails | every §15.2 criterion passes; `all-high` knee recorded in the release notes draft |

## Decisions taken at integration (formerly open questions)

1. **Layer pausing** (§11): on by default, with the switch `sfu.pause_unwatched_layers`; the slice is cuttable to M5.
2. **Who may run the connection test** (§7.6): every signed-in user, 3 concurrent probes per user; only admins see
   provider fix text (05).
