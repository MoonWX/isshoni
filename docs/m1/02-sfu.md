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
| Concurrency | One actor goroutine per Conn does all PeerConnection signaling calls. Media (RTP and RTCP writes included) runs on per-layer and per-DownTrack goroutines. No lock is held across Pion calls or callbacks, and no actor waits on another (§5.4) | Pion calls back from its own goroutines. An actor removes the deadlock and ordering bugs the spike's shared mutexes invited |
| Simulcast rids | `"f"` (full) and `"q"` (preview). `"h"` is reserved for the M5 720p30 middle layer and rejected in M1 | One byte per RTP packet for the rid extension. Same convention as LiveKit and the spike |
| Keyframes | A layer switch or resume happens only on a packet carrying an SPS (single NAL or STAP-A). PLI to a publisher is throttled to 1 per 500 ms per layer | S4: switching elsewhere leaves the decoder without the new resolution's SPS/PPS |
| Retransmission | One packet cache per incoming layer, sized to about 1 s at the measured packet rate, at least 1024 slots for video. NACKs from every viewer (video and audio) are served from it, as RTX when negotiated. No Pion NACK responder on the subscribe side | Memory does not grow with the number of viewers |
| Sender reports | Each DownTrack forwards the publisher's SR: NTP unchanged, RTP shifted by the DownTrack's current offset. The subscribe side has no SR generator | Keeps the publisher's capture clock end to end (A/V target under 45 ms) |
| Layer-switch timestamps | Aligned through the two layers' SRs. Wall-clock fallback | The output timeline stays continuous in capture time, not arrival time |
| Downlink | Explicit `subscribe.update` (high/low/off plus audio on/off), plus a server downgrade on a falling REMB (overuse) or loss (Galene-style) with timed trial upgrades and a notice to the viewer | Chrome's REMB stays below about 1.5× the received rate and falls only on overuse, so it can signal congestion but never grant an upgrade; upgrades have to be tried (§10.2) |
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
  doc.go          overview, lock order, goroutine table and its rules (copy of §5.4)
  sfu.go          SFU, New, Close, Ready, Join, room reads, StopShare, CloseRoom, ticker
  config.go       Config, Limits, defaults, validation
  api.go          the webrtc.API instances: pub (one per pub PC, §8.1), sub, probe (MediaEngines, interceptors,
                  SettingEngine on top of 04's netx.Transport.Apply), remote-candidate filter
  room.go         Room: participants, shares, codec policy
  participant.go  Participant
  conn.go         Conn actor: command queue, internal event queue, Resync, Close
  pubpc.go        publish PC: offer handling, validation, answer filtering, OnTrack → Layer
  pubnack.go      the pub PC's NACK filter: no NACK to the publisher for what a Layer has cached (§8.1, §9.5)
  subpc.go        subscribe PC: negotiation state machine, ICE restart, rebuild, DTLS gate
  subscription.go Subscription {video, audio DownTrack}, UpdateSubscriptions
  share.go        Share: lifecycle, layers, fan-out lists, keyframe requests, ShareInfo
  layer.go        Layer: RTP/RTCP read loops, SR store, rate and loss estimate, SPS info
  packetcache.go  per-layer ring cache
  downtrack.go    DownTrack (webrtc.TrackLocal): Bind, queue, writer, RTCP reader
  munger.go       seq/ts rewrite, epochs (the sequence map), SR-aligned switching, padding gaps
  rtx.go          NACK handling, RTX packets, retransmission budget
  pacer.go        per-DownTrack token bucket (video, retransmissions included; README S84)
  allocator.go    per-subscriber downlink controller, and what the viewer's REMBs and receiver reports tell it
                  (README S84)
  uplink.go       UplinkPolicy (M1: admin cap and layer pausing; M2: native estimator), quality hints, the REMB of
                  the cap on the pub PC (README S88)
  codec.go        ProfileKey, compatibility matrix, PT table, codec policy helpers
  codecstate.go   SetDecodeCaps, viewers without H.264: caps-driven video, codec rebuilds, the guard (§8.5)
  h264.go         SPS detection (keyframe start), SPS parser (size), NAL helpers
  sdpcheck.go     publish-offer validation, subscribe-answer checks, Opus fmtp edit (pion/sdp)
  probe.go        connection-test probe PCs
  events.go       Signaler, RoomEvents, Event types
  errors.go       Error and codes
  stats.go        ShareInfo, ConnStats, Snapshot, Metrics
  counters.go     the SFU's totals of §13 that Layers and DownTracks count into (README S79)
  pionlog.go      Pion's logger factory: info lines and lines that hold an address at debug (README S79, §13)
  *_test.go
internal/server/sfu/sfutest/   Harness, DirectSignaler, Viewer (+Recorder, checks), FaultConn, ProfileSource, Link
                               (a downlink bottleneck on the SFU's FaultConn, README S84), ApplyHint (a quality
                               hint's active flags on a publish.Publisher, README S88)
internal/server/sfu/sfutest/rembsim/  REMB model copying libwebrtc's AIMD estimator (no imports of sfu or Pion;
                                      used by allocator_test and sfutest.Viewer, §17)
internal/media/fake/           Source, synthetic and decodable H.264, Opus asset, testdata/
internal/client/publish/       Publisher: simulcast TrackLocal with mid/rid, H.264/Opus packetizing, SRs, PLI,
                               SetActive (a layer off and on again, README S88)
cmd/isshoni-loadtest/          main.go, auth.go, client.go (the simulated clients, README S89), scenario.go,
                               measure.go, report.go
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
| `Room` | SFU | participants, shares, codec policy | last Conn gone and no shares (a Conn's shares end with it, so the room goes with its last Conn); `CloseRoom`, which closes those Conns (§6.1) |
| `Participant` | Room | conns, shares (≤4) | last Conn closed |
| `Conn` | Participant | ≤1 pubPC, ≤1 subPC, subscriptions, pending ICE, negotiation state | `Close` (signal: grace expired, leave, revocation, room closed); `CloseRoom`; SFU shutdown |
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
  maps tracks to shares by that binding, read afresh on every offer. A sending m-section without a binding, or bound
  to a share that isn't a `pending`/`live`/`stalled` share of this Conn (the share ended while the offer was in
  flight: the hub drops such TrackRefs, and the SFU treats one it still gets the same way, 01 §9 rule 4), carries no
  share and is answered `a=inactive` (§8.4). A rebuilt pub PC has new mids but lists the same `shareId`s, so its
  tracks attach to the existing Shares. The msid stream id the browser chose is ignored.
- Rids: `f` = full (source, ≤1080p60 by default), `q` = preview (360p15, about 0.3 Mbps). In M1 a video m-line carries
  a subset of {`f`, `q`} (at most 2 rids) or no rids, which means a single `f` layer. Any other rid is rejected with
  `sfu.bad_rid`, and so is `h` (medium): it is accepted only once the M5 `layer.mid` feature exists.

### 5.3 Lifecycles

**Share** (M1). The SFU reports media facts; the hub (01 §4.4) owns the lifecycle, the 30 s `starting` and `stalled`
timeouts (`media_timeout`) and the end reasons:

```
StartShare ─► pending ──(first SPS keyframe on any video layer)──► live ◄──────────────► stalled
                                                                         pub PC not connected ≥ 2 s, or its
                                                                         video layers are gone (pub PC closed,
                                                                         failed or replaced by a new gen); back to
                                                                         live on the first keyframe on any bound
                                                                         video layer once the pub PC is connected
                                                                         again (same PC recovering, ICE restart,
                                                                         or a new gen's tracks)
Any state, only on a call from signal: StopShare(r) · source Conn Close(r) · CloseRoom(r) · SFU Close(server_shutdown)
  → the Share is removed and ShareEnded(r) fires; r is 01's EndReason string
```

`RoomEvents.ShareUpdated` fires on every state change and on layer, profile or audio changes while live; a layer
change is a track attaching or ending, never an `Active` flip from pausing (§11). The hub
turns them into `live`/`stalled` in `room.state`, sends Web Push ("X started streaming") on the first `live` only,
so aborted shares never notify (04 push, 01), and ends shares that stay `pending` or `stalled` for 30 s.

**Subscribe PC negotiation** (server is always the offerer on `sub`; the client on `pub`; so there is never glare):

| State | Event | Action → next |
|---|---|---|
| `idle` | change (track added/removed, ICE restart asked and not skipped, below) | debounce 50 ms (01 §9), `CreateOffer` → `SetLocalDescription` → wait for gathering (≤2 s) → `SendOffer(sub, gen, neg=n, sdp, tracks)` → `offering` |
| `offering` | change | set `dirty` |
| `offering` | answer with this `gen` and `neg == n` | check answer (§8.5), `SetRemoteDescription`, flush buffered candidates → `idle`; if `dirty`, offer again (`neg + 1`) |
| `offering` | answer with another `gen` or `neg` | ignore, log at debug |
| `offering` | every 15 s without an answer | send the same offer again (same `gen` and `neg`) |
| `idle`, `offering` | `Resync()` | if `offering`, send the same offer again; ICE-restart a sub PC that isn't `connected` (01 §10.5), unless an ICE restart is already under way (below) |
| any | `ResetPC(sub)` or a codec rebuild (§8.5) | close PC; new PC with `gen + 1`, `neg = 1`; add all DownTracks again; offer → `offering` |
| `closed` (closed by the SFU after the `failed` grace or a fatal error, by Pion because the client closed its side (below), or never built), while the Conn has subscriptions | `ResetPC`, `RestartICE`, `Resync()` or a subscription change | new PC with `gen + 1`, `neg = 1`; add all DownTracks; offer → `offering` |

`gen` and `neg` are 01's counters (01 §9): per Conn and PC kind, `gen` counts PC generations (the SFU increments it
for `sub`, the client for `pub`) and `neg` counts offers within a `gen`. The SFU is the only owner of this state for
both PC kinds; the hub keeps no PC counters and checks only role and ownership (01 §21). A pub offer with a higher
`gen` replaces the pub PC. One with a lower `gen`, or with a `neg` lower than the last one in the current `gen`,
returns `sfu.stale_offer` and changes nothing (sfuplane sends `stale_negotiation`, which the client ignores). A
repeated pub `neg` gets the stored last answer again. `RestartICE`, `ResetPC` and `ClosePC` with a `gen` lower than
the current one for that kind do nothing and return nil.

**One sub ICE restart at a time.** An ICE restart of the sub PC (from `RestartICE` or `Resync()`) is skipped when one
for this `gen` is already queued, or its offer was sent less than 5 s ago, and ICE has neither connected nor failed
since. `RestartICE` then returns nil and sends no new offer. This is the usual case after a network switch (Wi-Fi to
LTE): the client's sub PC has been `disconnected` for 3 s when the WebSocket resumes, so `Resync()` and the client's
own `pc.restart{sub, ice}` both ask for a restart. A second restart would throw away the checks already under way. The
same rule covers the Go client. The client treats the sub offer with a new `ice-ufrag` as the restart and starts its
15 s rebuild timer from it (05 §9, 01 §10.4).

**PeerConnection state** (both kinds):
- New PC: a 10 s handshake timer (ICE + DTLS to `connected`); expiry closes it and sends
  `PCStateEvent{failed, "handshake_timeout"}`. ICE restarts have no server timer and send no event: the client
  rebuilds after 15 s (01 §10.4).

  **The timer starts with the PC's first answer, not with the PC** (README S57, `Conn.startHandshake`): the moment
  the client can start ICE and DTLS. For a pub PC that is when `HandleOffer` has taken the first offer of its
  `gen`, just before it returns the answer. For a sub PC it is when `HandleAnswer` has applied the first answer
  to an offer of its `gen`; a sub offer that is still waiting for its answer is not timed, however often the 15 s
  re-send repeats it, because a client that hasn't answered has no ICE to complete. Later answers on the same PC
  start nothing. The timer stops when the PC is `connected` and when its ICE fails first (the grace below takes
  over), and with the PC. When it runs out the PC is closed and counted (`Metrics.HandshakeTimeouts`, §13):
  - **pub**: the PC is gone and its `gen` is over, as after a client's close (below). Its shares stay as they
    were, `pending` for a first PC, until the hub's 30 s timeout or the next `gen`'s keyframe; the event has the
    client offer `gen + 1` (§6.4).
  - **sub**: the PC is in the `closed` state of the table above, with the Conn's subscriptions kept; the
    client's rebuild request brings the successor.
- `connected`: sub PC opens the DTLS gate and sends a keyframe request for every video DownTrack. Pub PC (again
  `connected` after a drop, an ICE restart or a rebuild): one keyframe request per video layer of its shares (§9.7).
- `disconnected`: nothing. The client drives restarts (01/05: ICE `disconnected` for 3 s → restart).
- `failed`: send `PCStateEvent`, start a 30 s PC grace. With no restart or reset by then, close the PC and send
  `PCStateEvent{closed, "pc_grace_expired"}`. The grace ends without that when the PC is `connecting` again (an
  ICE restart got in) or is replaced. For pub, its shares are `stalled` at once and stay so until the pub PC is
  connected again and a keyframe arrives (Share diagram above), or the hub's 30 s timeout ends them.
- `closed` without a call from signal: Pion closes a PeerConnection when its peer closes its side (the DTLS
  `close_notify` alert), so the SFU sees a current PC go `closed` with no `pc.close` message behind it. A client
  that closes before its own DTLS side is connected sends no alert; the SFU then only sees ICE fail. S29 keeps
  Pion's behaviour and reports it: `PCStateEvent{closed}`, for the current PC only (a PC that the SFU replaced or
  tore down is no longer current when its `closed` arrives, and sends nothing).
  - **pub**: the PC is gone. Its tracks have ended, so its shares lose their layers but live on (the hub owns their
    lifecycle and its 30 s timeout). A later offer with that `gen` returns `sfu.bad_pc`; only an offer with a higher
    `gen` brings a new pub PC.
  - **sub**: the PC stays the Conn's sub PC, in the `closed` state of the table above, and the Conn keeps its
    subscriptions. Nothing more is offered on it, an answer for its `gen` returns `sfu.bad_pc`, and a subscription
    whose share ends is removed without an offer. The same state follows a **fatal error**: a sub offer that Pion
    can't create or set (the client gets `ErrorEvent{sfu.internal, pc.sub}`), or an answer that Pion refuses after
    taking it (`HandleAnswer` returns `sfu.bad_sdp`; Pion has no rollback, so that PC can't negotiate again).
    The successor (`gen + 1`) is built when the Conn needs a sub PC again (the `closed` row of the table above,
    README S57); the subscriptions wait until then, and one made meanwhile joins them.

  **Decided in README S57: Pion's close on `close_notify` stays.** The alternative,
  `SettingEngine.DisableCloseByDTLS`, was tried: after the client's close, Pion goes on reporting the
  PeerConnection and its DTLS transport as `connected` and takes every packet written to it without an error, until
  ICE notices (5 s to `disconnected`, 20 s to `failed`) and the 30 s grace has passed. For that long the DTLS-ready
  gate would stay open on a dead sub PC, and a share would stay `live` for about 7 s without its publisher. With
  the close the media facts are right at once: a pub PC's shares are `stalled` when its tracks end, a sub PC's
  DownTracks stop, and the PC's ICE user name leaves the muxes. `ClosePC` (the `pc.close` path) therefore often
  finds its PC closed already, since the alert travels on the media path and `pc.close` through the hub; it returns
  nil either way, and a PC that it closes itself sends no `PCStateEvent`. The same two tests pin this
  (`TestSubPCClosedByClient`, and the end of `TestPubOfferGenAndNeg`); `TestSubPCRebuild` has the ways out of
  `closed`.
- PCStateEvents are sent only for the current PC of each kind, never for one already replaced by a higher `gen`.
- The SFU never starts an ICE restart or a rebuild because of its own ICE state. Its only unsolicited actions are the
  pub rebuild request (via `PCStateEvent`), the sub ICE restart in `Resync()`, and the codec rebuilds of §8.5
  (01 §10.4). The client drives every other recovery of both PCs.

**Conn**: `active` → (`Close`) → `closed`. There is no "detached" state in the SFU: a WebSocket drop is invisible to it.
Signal keeps the `Signaler` alive, calls `Resync()` on resume and, after the 30 s grace, `StopShare(id, disconnected)`
for each of its shares, then `Close` (plan). The reason passed to `Close` reaches only logs, because the hub has
already ended every share with its own reason.

### 5.4 Goroutines and locks

| Goroutine | Count | Does | Waits on |
|---|---|---|---|
| Conn actor | 1 per Conn | every signaling operation and every Pion PC signaling call (SRD, SLD, CreateOffer/Answer, AddTrack, RemoveTrack, Close); handles Pion callbacks, cross-Conn notifications and ticker work | command queue (cap 64, signal calls only) + internal event queue (unbounded: Pion callbacks, cross-Conn and timer work) |
| Layer RTP reader | 1 per incoming layer | `Read` → parse → cache insert → fan-out to interested DownTracks | Pion read |
| Layer RTCP reader | 1 per incoming layer | `ReadSimulcastRTCP(rid)` / `ReadRTCP()` → SR store, SR items to DownTracks | Pion read |
| DownTrack writer | 1 per DownTrack | RTX first, then munge, pace, write RTP, forward SRs | its queues |
| DownTrack RTCP reader | 1 per DownTrack | PLI/FIR, NACK, REMB, RR from the viewer | `RTPSender.ReadRTCP` |
| Ticker | 1 per SFU | 250 ms: event debounce (the `ShareUpdated` calls; `SubscriptionStateEvent`s are paced by their Conn's actor, below). 1 s: rates, cache sizing, stalled checks, allocator ticks, codec-policy timers, uplink policy | time |
| Probe | 1 per probe PC, for its life (README S76: `probe.run`) | owns the probe PC once it has started: notes when it connects and reports the result once (1 s later, or when the probe ends first); ends the probe at the end of its 20 s life, when the PC fails or closes, when its data channel closes, or when it is stopped (a newer probe of the same user and transport, `SFU.Close`); then closes the PC and frees the slot. `SFU.Close` stops every probe, replaced ones included, and waits for them within its 1 s, then counts those still closing in one warning | its timers (life, the RTT sample) + a wake of capacity 1 that Pion's state callback fills without blocking + the channel's close + its stop. The echo is no goroutine of ours: it runs in Pion's data-channel read loop, and a send only queues (§7.6) |

Rules:
- Lock order: `SFU.mu` → `Room.mu` → `Share.mu` → `DownTrack.mu`. Each is held briefly and never across a Pion call,
  a `Signaler`/`RoomEvents` call or a blocking channel send. `Room.mu` also guards each Conn's decode caps, which
  the room's policy reads (README S69).
- **Two exceptions: `Share.notify` and `Room.policyMu`.** Each is a mutex outside the order above, and they are
  the only locks held across a `RoomEvents` call.
  - **`Share.notify`** (README S41), one per share, only puts a share's reports in order: the ticker holds it while
    it makes the share's `ShareUpdated` calls, and `SFU.endShare` holds it while it ends the share, reports the
    state changes the ticker hadn't reported yet and then `ShareEnded`. So every `ShareUpdated` comes before the
    `ShareEnded`, and nothing is reported about a share after its end (§6.2). It is safe because `RoomEvents` calls
    never block (the last rule below), `Share.mu` is the only lock taken inside it, and whoever takes it holds no
    other lock.
  - **`Room.policyMu`** (README S69), one per room, only puts the room's codec policy changes and their reports in
    order (§8.3). `SFU.updateCodecPolicy` holds it while it works out the policy (under `Room.mu`, taken inside
    it), makes the change's `CodecPolicyChanged` call and posts one `CodecPolicyEvent` per share to the internal
    event queue of the Conn that publishes it. So one evaluation runs at a time, and its reports are out before the
    next one starts: every publisher hears of the changes in the order they happened. Its callers are `Join`, a
    Conn's close, `SetDecodeCaps` and the ticker, none of which holds a lock then. It is safe for the reasons
    `Share.notify` is: the `RoomEvents` call and the posts never block, `Room.mu` is the only lock taken inside it,
    and whoever takes it holds no other lock.
- **The layer choice is made under `Share.mu`, then `DownTrack.mu`** (README S52; the order above). Which layer a
  DownTrack forwards (§10.1) is chosen with both held:
  - by the subscriber's actor when the request or the server's cap changes (`DownTrack.setWant`);
  - by whoever attaches or detaches a layer of the share, before that returns (`Share.attach`, `Share.detach`,
    which walk the share's DownTracks: `retargetLocked`). That is the publishing Conn's actor both times: for a
    track that arrived, and for one whose reader posted its end.

  So the choice always fits the layers the share has, and a DownTrack already wants a new layer when its track's
  first packet is read: a viewer who was subscribed before the layer arrived gets it from its first keyframe,
  without a PLI.
- **What a subscription gets is reported by the subscriber's actor alone, from a notice** (README S52). Whoever
  changes it elsewhere (a DownTrack writer whose stream began, ended or moved to another layer; Pion's `Unbind`;
  an `attach` or `detach`) releases its locks first and then calls `Subscription.changed`, which posts to that
  actor's internal event queue and never blocks. Two bounds keep a publisher from flooding its viewers:
  - **One notice per subscription.** An atomic flag says that a notice is queued and not looked at yet. The actor
    clears it before it looks, so a change that finds a notice queued is seen by that look, and one that comes
    later queues the next. A stream that begins and ends with every frame (a publisher that alternates between two
    profiles, §9.4) therefore adds one entry to the unbounded queue, not thousands a second.
  - **One event per 250 ms** for what the media path changed (`SubscriptionStateEvent`, §6.2). The wait is a timer
    per subscription whose callback posts to the actor again; it is not the SFU ticker's work.
- `Bind` and `Unbind` of a DownTrack are called by Pion with the sender's lock held. They read the negotiation and
  store the result, taking `DownTrack.mu` for a moment. That lock is never held across a Pion call, so the two are
  only ever taken in this order.
- Pion callbacks (`OnTrack`, `OnConnectionStateChange`, `OnICEConnectionStateChange`) only append to the Conn's
  unbounded internal event queue. They never block, so a Pion call made by the actor can't deadlock on its own
  callback.
- Cross-Conn and timer-originated work is posted to the target Conn's internal event queue, never to the bounded
  signal command queue, so no actor ever waits on another. Examples: a share ended on the publisher's actor → each
  subscriber's actor removes that subscription's DownTracks (`RemoveTrack`) and renegotiates; codec-retry rebuilds
  (§8.5); ticker actions that need a PC call. `StopShare` therefore returns once the Share is removed and the posts
  are queued, however busy the subscribers are.
- `WriteRTP` and `WriteRTCP` (PLI, SR, REMB) are media calls, not signaling calls. Media goroutines (DownTrack
  writers and RTCP readers, Layer readers, the ticker) make them, and so may any actor, including one writing to
  another Conn's pub PC (a keyframe request from `Bind` or on sub `connected`, §9.7). Pion allows them from any
  goroutine, and they never block: UDP goes through the mux, and 04's TCP muxes write through a 4 MiB per-connection
  buffer that drops when full. The 500 ms CAS throttle in `Share.requestKeyframe` (§9.7) keeps concurrent callers to
  one PLI.
- Signal calls block on the actor for at most 5 s (`ctx`). A full command queue after that returns `sfu.busy`
  (retryable).
- Fan-out lists (`Share.video`, `Share.audio`) are `atomic.Pointer[[]*DownTrack]`, copied on write, so the read path
  takes no lock.
- DownTrack munger state is guarded by `DownTrack.mu`. The writer goroutine takes it once per packet (uncontended).
  Control calls take it briefly. As built there is one, `setWant`: the subscription works out `min(requested, cap)`
  itself and hands the DownTrack the result (§10.1), so the `setRequested` and `setCap` of the first design are
  one call.
- `Signaler` and `RoomEvents` implementations must not block and must not call into the SFU synchronously.
- **A share's uplink control belongs to the publishing Conn's actor** (README S88, §11): its policy, the cap and
  pause state its sharer was told, and the pub PC's REMB state. The ticker and `SetLimits` only post to that actor,
  a look of all its shares queued at most once at a time (`Conn.upQueued`, `wakeUplink`); a viewer's new request for
  `high` posts a look of that one share (`Share.askedForHigh`), which writes no REMB. What other goroutines
  need of it are atomics: `Share.paused` (a keyframe request for a paused layer does nothing, §9.7) and
  `Share.resumedAt`, which the actor stores before it clears a bit of `Share.paused`, so a downlink allocator never
  sees a resumed layer as neither paused nor resumed (§10.2). The viewers' requests reach the policy as each
  subscriber's actor published them (`Subscription.shown`); a request for `high` has that actor post a look after
  it published it, so the look sees it.

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

// The first three start at 1 and have no valid zero value; the next four start at 0 ("Enum values" below).
type Role uint8       // RoleFull (publish+subscribe) | RoleViewer | RolePublisher (M2 desktop core) | RoleAgent (M4)
type ClientKind uint8 // ClientWeb | ClientDesktop | ClientMobile (later)
type PCKind uint8     // PCPub (client offers) | PCSub (server offers)
type Quality uint8    // QualityOff | QualityLow | QualityHigh; later (M5): QualityMedium ("h")
type Preset uint8     // PresetAuto | PresetGame | PresetMovie | PresetText
type SourceKind uint8 // SourceUnknown | SourceScreen | SourceWindow | SourceTab
type ShareState uint8 // SharePending | ShareLive | ShareStalled (media facts only; §5.3)
type EndReason string // 01's protocol.EndReason values, passed through: "stopped" "left" "disconnected"
                      // "media_timeout" "room_closed" "server_shutdown" ("kicked" reserved); constants
                      // EndReasonStopped … EndReasonServerShutdown
type SubReason string // "" "bandwidth" "server_limit" (later) "codec_mismatch" "decoder_unavailable"
                      // "decoder_failed" "no_preview_layer" "no_layer"; 01 §15.4 maps them to the wire

type DecodeCaps struct {
	H264 []ProfileKey // what the viewer can decode; empty = no H.264 decoder (yet)
}

// ---- construction (called by 04's wiring, internal/server) ----
func New(cfg Config, deps Deps) (*SFU, error) // builds the APIs on cfg.Transport (sockets are 04's, already bound)
func (s *SFU) Close() error                    // ends every share (server_shutdown); closes all PCs concurrently and
                                               // returns within 1 s (PCs still closing then are abandoned, logged at
                                               // warn); safe to call twice; never closes the Transport (04's)
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
func (s *SFU) CloseRoom(room RoomID, r EndReason)      // ends the room's shares with r, then closes its Conns
                                                       // (Conn.Close(r)); for callers without a hub (below)
func (s *SFU) SetLimits(l Limits)                    // admin soft limits, applied live (wiring: 03 settings OnChange)
// later: func (s *SFU) SetRoomLimits(room RoomID, l RoomLimits)
func (s *SFU) Snapshot() Snapshot                    // §13
func (s *SFU) Metrics() Metrics                      // §13
func (s *SFU) Probe(ctx context.Context, user UserID, t ProbeTransport, offerSDP string) (string, <-chan ProbeResult, error) // §7.6

// ---- per connection (called by 01's sfuplane from the hub's connection actor; serialized by the Conn actor) ----
func (c *Conn) ID() ConnID
// HandleOffer applies a pub offer and returns the answer SDP synchronously (gathering is instant with muxes).
// A higher gen replaces the pub PC (the client rebuilt it); tracks binds every sending m-section to a share.
// A lower gen, or a lower neg in the current gen, returns sfu.stale_offer (§5.3). An 11th new gen within a minute
// returns sfu.pc_rate_limited, and a PCKind beyond the two sfu.pc_limit ("PC limits" below).
func (c *Conn) HandleOffer(ctx context.Context, pc PCKind, gen, neg uint32, sdp string, tracks []TrackBinding) (answerSDP string, err error) // PCPub only
func (c *Conn) HandleAnswer(ctx context.Context, pc PCKind, gen, neg uint32, sdp string) error // PCSub only
func (c *Conn) AddICECandidate(ctx context.Context, pc PCKind, gen uint32, cand webrtc.ICECandidateInit) error // filtered, §7.3
// For RestartICE, ResetPC and ClosePC, a gen lower than the current one for that kind is ignored (returns nil,
// logged at debug).
func (c *Conn) RestartICE(ctx context.Context, pc PCKind, gen uint32) error // PCSub only (for pub the client re-offers);
                                                                            // a closed sub PC is rebuilt instead, and
                                                                            // that can be sfu.pc_rate_limited; nil and
                                                                            // no new offer while one is under way
                                                                            // (§5.3)
func (c *Conn) ResetPC(ctx context.Context, pc PCKind, gen uint32) error    // PCSub: close and rebuild with gen + 1 (client asked)
                                                                            // sfu.pc_rate_limited: the 11th in a minute
func (c *Conn) ClosePC(ctx context.Context, pc PCKind, gen uint32) error    // the client closed a PC on purpose (pc.close)
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

**Enum values** (S29). `Role`, `ClientKind` and `PCKind` start at 1 (`iota + 1`), so they have no valid zero value: a
value that the adapter forgot to set is refused instead of meaning `full`, `web` or `pub`. `Join` refuses a `Role`
or `ClientKind` outside its constants as a contract violation (§6.3), and `HandleOffer`, `HandleAnswer` and
`AddICECandidate` return `sfu.bad_pc` for a `PCKind` that isn't theirs, the zero value included (for a kind above
`PCSub`, `HandleOffer` returns `sfu.pc_limit` instead: "PC limits" below). `Quality`,
`Preset`, `SourceKind` and `ShareState` start at 0, where the zero value is the natural default: `QualityOff`,
`PresetAuto`, `SourceUnknown`, `SharePending`. The numbers are this package's own and may change: sfuplane maps
every enum by name (01 §15.4). Each of the seven types has a `String()` with the lower-case name (`full`, `web`,
`pub`, `high`, `auto`, `window`, `live`, …); for `Role`, `PCKind`, `Quality` and `ShareState` these are the
`Metrics` label values of §13.

**PC limits** (README S57; the numbers and what counts are §12's).
- **`sfu.pc_limit`** comes from `HandleOffer` alone, for a `PCKind` above `PCSub`: a Conn has one pub and one sub
  PC, and such an offer would be for a third. A second PC of one kind can't be asked for at all, because an offer
  of a higher `gen` replaces the pub PC and the SFU builds every sub PC itself, so that half of the code's meaning
  has no case. `PCSub` and the zero value stay `sfu.bad_pc` (above).
- **`sfu.pc_rate_limited`** comes from every request that would make the Conn create one client-caused PC too
  many, with the time to wait in `RetryAfter`:
  - as the call's error from `HandleOffer` (a new pub `gen`), from `ResetPC`, and from **`RestartICE`** when the
    sub PC is closed, since it then rebuilds the PC as `ResetPC` would (§5.3);
  - as `ErrorEvent{sfu.pc_rate_limited, "pc.sub"}` for the two requests that rebuild a closed sub PC and have no
    error to put it in: `Resync()`, which returns nothing, and `UpdateSubscriptions`, whose items are applied
    whether or not the PC that will carry them can be built now. A sub PC that Pion can't create on those two
    paths is reported the same way, as `ErrorEvent{sfu.internal, "pc.sub"}`.

  A refused request changes nothing: the pub PC of the old `gen` stays, a sub PC that `ResetPC` was to replace
  stays as it is, and a closed one stays closed with its subscriptions waiting. Nothing builds the refused PC
  when the minute has passed, so the client asks again; the event is how it knows after a resume or a
  subscription change.

**`CloseRoom`** (S29) ends every share of the room with `r` (`ShareEnded(r)` each, in `StartedAt` order) and then
closes every Conn of the room with `Close(r)`; the room is removed with its last Conn. It is the direct way for a
caller without a hub (tests, tools). The hub does not use it: a deleted room reaches the SFU through the hub's
connections, each of which ends its shares with its own reason and closes its peer (`Conn.Close`; §19 here,
01 §15.2). `Conn.Close` is idempotent, so the two ways never conflict. An unknown room is a no-op.

**`StartShare` and `UpdateShare`** exist from S29 on with the numbers of §8.6 (they are not stubs until slice 7):
- `StartShare` creates the share `pending` under the hub's id and returns `ShareParams` for its preset: `f` then `q`,
  both `Active`, `f.MaxBitrate` under the admin cap, `AudioBitrate` by preset, and `Profile` = the room's codec
  policy (`6400` for every room until slice 10, README S69, added the policy). Errors: `sfu.role_forbidden`,
  `sfu.too_many_shares`, and the contract violations of §6.3 (an empty or reused share id, an unknown preset).
- `UpdateShare(id, ShareUpdate{Preset})` stores the new preset and returns the share's new `ShareParams`; a nil
  `Preset` changes nothing and returns the current ones. The encodings apply at once (the client sets them); the
  audio bitrate reaches the publisher with the next pub offer's answer (§8.4 step 5 reads the share's preset when it
  writes `maxaveragebitrate`). Errors: `sfu.share_not_found`, `sfu.not_owner`. **Since README S88 its encodings are
  the share's as they stand**: under the admin's cap as it is now, and with the layers inactive that layer pausing
  told the sharer to pause (§11), so a sharer that applies them pauses or resumes nothing behind the SFU's back.
  The uplink control doesn't count them as told: the hub also runs another of the user's connections'
  `share.update` on the publishing Conn and replies to that connection (01 §8.7), so the sharer may never see them,
  and a cap that differs from what its last hint said still gets a hint at the next look (review S88-R4).
- `SetLimits` replaces the cap for every `ShareParams` returned from then on; an invalid value (a negative number)
  is logged and ignored. Since README S88 running shares follow at once too, not at the ticker's next second: every
  Conn that publishes looks at its uplink, so a share whose cap changed gets a `QualityHintEvent` with the reason
  `admin` and its pub PC the REMB of the new cap (§11). A cap beyond 2^30 kbit/s counts as that much
  (`Limits.shareCap`), so the REMB of a pub PC with several shares can't overflow.

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

type SubscriptionStateEvent struct { // to the subscriber; only when Forwarded, Audio or Reason changes
	Share     ShareID
	Requested Quality
	Forwarded Quality   // what the SFU actually sends
	Audio     bool      // audio actually forwarded
	Reason    SubReason // why Forwarded < Requested. "" when equal, or while Forwarded < Requested only because the
	                    // sub PC isn't connected yet or the target layer's keyframe hasn't arrived (01 maps it to
	                    // `waiting`)
}
type CodecPolicyEvent struct { // to the Conn that publishes Share: re-offer with this profile first (§8.3)
	Share   ShareID
	Profile ProfileKey
}
type QualityHintEvent struct { // to the publishing Conn (§11)
	Share      ShareID
	Reason     string           // "admin" | "viewers"
	MaxBitrate int              // bps; 0 = no cap. M1: admin cap. M2: native uplink estimator
	Encodings  []EncodingParams // the full current list, same shape StartShare returns, with the admin cap and the
	                            // pause state applied
}
type PCStateEvent struct {
	PC     PCKind
	Gen    uint32 // generation of the PC this state belongs to
	State  string // "connected" "disconnected" "failed" "closed"
	Reason string // "" | "handshake_timeout" | "pc_grace_expired"
}
type ErrorEvent struct {
	Err   *Error
	Scope string // "pc.pub" | "pc.sub" | "share" | "subscription"; sfuplane maps it to 01's scope and pc fields
}
```

The SFU computes the content of every hint; sfuplane only converts types (01 §15.4):
- A `CodecPolicyEvent` becomes a `quality.hint` that carries only `codec`: a profile switch doesn't change encodings.
  A `QualityHintEvent` becomes a `quality.hint` with the SFU's `reason` and full `encodings`.
- PCStateEvents are emitted only for the current PC of each kind, never for one already replaced by a higher `gen`.
  sfuplane copies `Gen` into `pc.restart`'s `gen`.
- On `Resync()` the SFU re-sends one `CodecPolicyEvent` per share the Conn publishes and the last `QualityHintEvent`
  per share, if one was sent. A client re-offers only when the hinted codec differs from the one it last applied (05),
  so the re-send costs nothing. **As README S88 built it** (`Conn.resendHints`), the quality hint is not the stored
  event sent again but **rebuilt as it would be now**: the current cap, the current pause state and the encodings of
  the current preset, with the reason of the last hint. A preset changed after the hint would otherwise be undone by
  the old encodings. A share that never got a hint gets none.
- `QualityHintEvent.Reason` is one of the exported constants `HintReasonAdmin` (`"admin"`) and `HintReasonViewers`
  (`"viewers"`), 01's `protocol.HintReason` values, which sfuplane passes through (README S88).

**`SubscriptionStateEvent` as README S52 built it** (`subscription.go`; the layer choice itself is §10.1).

- **When.** The subscriber's actor compares what the subscription gets now (`Forwarded`, `Audio`, `Reason`) with
  what the client last heard and sends an event only when they differ: never for a request alone, and not for a
  new subscription that forwards nothing yet without a reason, which the client knows without being told. A
  subscription that is gone, or whose share has ended, reports nothing more: the hub tells clients that a share
  ended.
- **`Requested`** is the client's latest request, whatever the server's cap.
- **`Forwarded`** is the layer the viewer gets at this moment: `high` for the full layer `f`, `low` for the
  preview layer `q` (and for the M5 middle layer `h`, until a `QualityMedium` exists), `off` for none. It follows
  the stream, not the target: a switch is reported when the new layer's keyframe has arrived, so for the moment a
  switch down takes `Forwarded` is above `Requested`, and a viewer whose stream has not begun (it waits for its
  sub PC, or for the first keyframe) has `off`.
- **`Audio`** is true while the client asked for the share's audio and it flows to this viewer.
- **`Reason`** is set only while `Forwarded < Requested`, and then says why time alone won't change that:
  1. the server's cap, with the cap's own reason, when the cap is below the request (§10.1; no cap is set before
     slices 10 and 11);
  2. else `""` when the share has a layer that serves the request in full: what the viewer still lacks is on its
     way (the sub PC's connection, the layer's keyframe; 01 maps this to `waiting`);
  3. else `no_preview_layer` for a request of `low`, and `no_layer` for a request of `high`.

  So `no_layer` does not imply `Forwarded: off`: a request for `high` that gets the preview layer, because the
  share has no fuller one, is `Forwarded: low` with `no_layer`; one that gets nothing, because the share has no
  video layer yet, is `Forwarded: off` with `no_layer`. 01 maps both reasons to `unavailable`.
- **At once, or paced at 250 ms.** A publisher decides how often what its viewers get changes; it must not decide
  how many events they are sent.
  - What the client asked for itself is reported **at once**, before `UpdateSubscriptions` returns: a pause, and a
    request that the share's layers can't serve. So is a cap the server sets (`capSubscription`).
  - What the **media path** changed (a layer came or went, a DownTrack's stream began, ended or moved to another
    layer) is reported **at most once per 250 ms and subscription** (`subEventInterval`). A change that comes
    sooner after the subscription's last event, whichever kind that was, waits for the rest of the interval, and
    what is sent then is the state at that time. The client always ends up knowing the latest state, never hears
    more than four unasked states a second, and states that came and went in between are left out.
  - A report that is waiting is dropped when its subscription goes.
- The interval is not the ticker's (§5.4): each subscription has its own timer, and a subscription has at most one
  notice in its actor's queue.

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
	Layers      []LayerInfo     // sorted f, h, q; every layer whose track is attached, paused ones included (§10.1)
	Audio       bool            // an audio track is attached
	Viewers     []ParticipantID // sorted; forwarding now (debug and tests). room.state watchers and the dashboard's
	                            // per-participant `watching` come from the hub (01 §4.1); the dashboard's per-share
	                            // counts from ShareSnapshot.Viewers (§13)
	StartedAt   time.Time
	LiveAt      time.Time       // zero until the first keyframe
}
type LayerInfo struct {
	RID           string
	Width, Height int     // from the last SPS
	FPS           float64 // measured
	Bitrate       int     // bps, 2 s EWMA
	LossPct       float64 // ingress loss over the last 2 s
	Active        bool    // packets in the last 2 s
}
```

### 6.3 Errors

```go
type Error struct {
	Code       string        // stable; never sent on the wire: 01's sfuplane maps it to a protocol ErrorCode (01 §15.4)
	Retryable  bool
	Share      ShareID       // set for no_h264, unknown_track and bad_rid when the failing m-section maps to a share
	RetryAfter time.Duration // set for pc_rate_limited (§12), busy (1 s) and probe_limit (until the oldest running
	                         // probe's 20 s lifetime ends, never below 1 s)
	msg        string        // logs only, never sent
}
func (e *Error) Error() string          // the code, and ": " + msg when there is one
func (e *Error) Is(target error) bool   // errors.Is(err, &sfu.Error{Code: sfu.CodeBadSDP}) matches by Code alone

// One constant per code of the table below: CodeClosed = "sfu.closed", CodeBusy, CodeRoleForbidden, CodeBadPC,
// CodeBadSDP, CodeNoH264, CodeBadRID, CodeUnknownTrack, CodeStaleAnswer, CodeStaleOffer, CodePCLimit,
// CodePCRateLimited, CodeShareNotFound, CodeNotOwner, CodeTooManyShares, CodeTooManySubscriptions, CodeProbeLimit,
// CodeTransportDisabled, CodeInternal.
```

Every exported method that can fail returns an `*Error` (`New` returns plain errors: a nil `Config.Transport`, a
negative limit). Callers match it with `errors.As` and switch on `Code`, or with `errors.Is` against an `*Error`
that has only `Code` set.

| Code | Returned by | Retryable | Meaning |
|---|---|---|---|
| `sfu.closed` | any Conn method; Probe | no | Conn or SFU closed. `Probe` checks `Ready()` before the transport, the offer and the slot (only a contract violation comes first, §7.6), so after `Close` it is `sfu.closed` even for a disabled transport or a bad offer; it is also the answer for a probe that the same user's next probe of the transport replaced before the call returned |
| `sfu.busy` | any Conn method | yes | actor queue full for 5 s |
| `sfu.role_forbidden` | StartShare, HandleOffer(pub), UpdateSubscriptions | no | role can't do that |
| `sfu.bad_pc` | HandleOffer, HandleAnswer, AddICECandidate, RestartICE, ResetPC, ClosePC | no | method not valid for that PC kind (e.g. an offer for `sub`, `RestartICE` or `ResetPC` for `pub`, an unknown kind; for `HandleOffer` a kind above `PCSub` is `sfu.pc_limit` instead), or the PC doesn't exist: a pub offer for a `gen` whose PC has closed, an answer for a closed sub PC's `gen` or without a sub PC, a `RestartICE`, `ResetPC` or `ClosePC{sub}` for a sub `gen` above the current one or without a sub PC. Not returned by `RestartICE`/`ResetPC` when the sub PC was closed: those calls rebuild it (§5.3) |
| `sfu.bad_sdp` | HandleOffer, HandleAnswer, Probe | no | unparsable SDP, data channel in a media PC, >8 m-lines, a pub offer >64 KiB or a sub answer >256 KiB (01 §3.3). For `Probe` (README S76; 04 answers 400 `bad_sdp`): an offer that is empty, over 64 KiB, unparsable, not exactly one m-section that is `m=application`, or one that Pion's `SetRemoteDescription` refuses (§7.6) |
| `sfu.no_h264` | HandleOffer | no | the video m-line offers no H.264 packetization-mode=1 (browser without H.264 encoder) |
| `sfu.bad_rid` | HandleOffer | no | a rid outside {f,q} (M1), or more than 2 |
| `sfu.unknown_track` | HandleOffer | no | a malformed binding: `tracks` binds one mid twice, a binding of this Conn's share has another kind than its m-section, or one share has a second video or audio m-section. Not for a sending m-section without a binding, or bound to a share that isn't a `pending`/`live`/`stalled` share of this Conn (a share that ended while its offer was in flight): §8.4 answers that m-section `a=inactive` and ignores it (01 §9 rule 4) |
| `sfu.stale_answer` | HandleAnswer | no | `gen`/`neg` don't match the outstanding offer (sfuplane drops it silently) |
| `sfu.stale_offer` | HandleOffer | no | pub offer with a `gen` lower than the current one, or a `neg` lower than the last one in the current `gen` (sfuplane sends `stale_negotiation`; the client ignores it) |
| `sfu.pc_limit` | HandleOffer | no | a third PC: an offer whose `PCKind` is above `PCSub`. A second PC of the same kind can't be asked for, a new `gen` replaces (§6.1 "PC limits") |
| `sfu.pc_rate_limited` | HandleOffer, ResetPC, RestartICE (of a closed sub PC); as an `ErrorEvent` with scope `pc.sub` for `Resync()` and `UpdateSubscriptions` | yes | more than 10 client-caused PC creations per PC kind in a minute (§12); nothing changes, and `RetryAfter` says when to ask again (§6.1 "PC limits") |
| `sfu.share_not_found` | StopShare, UpdateShare, UpdateSubscriptions item | no | unknown or ended share |
| `sfu.not_owner` | Conn.StopShare, UpdateShare | no | the share belongs to another Conn (the hub always calls the publishing Conn) |
| `sfu.too_many_shares` | StartShare | no | defense in depth: 4 per participant (the hub enforces 4 per user and the room soft limit first, 01 §8.7) |
| `sfu.too_many_subscriptions` | UpdateSubscriptions | no | guard: 256 per Conn, 64 items per call |
| `sfu.probe_limit` | Probe | yes | 20 probes per server already running (a user's new probe of the same transport replaces the old one, §7.6). `RetryAfter` runs until the oldest running probe's 20 s life ends and is never below 1 s: a probe whose life has just ended keeps its slot until its PC is closed (README S76). 04 answers 429 with `Retry-After` |
| `sfu.transport_disabled` | Probe | no | the requested transport has no mux: `udp` with `listen.ice_udp` = "", `tcp443` in `tls.mode=off`, `tcp7882` with `listen.ice_tcp` = "" (04 answers 409 `transport_disabled`) |
| `sfu.internal` | any | yes; no for the case below | unexpected Pion error (logged with details). Also, with `Retryable` false: a call that breaks the API's contract (and, until README S76, a method that wasn't implemented yet) |

**`sfu.internal` that is not retryable** (S29). Two kinds of error used the code with `Retryable: false`, because
trying again can't help; since README S76 only the first is left:
- **A contract violation**: the caller, not the client, got it wrong. `Join` with an empty `Room`, `Participant`,
  `User` or `Conn`, a `Role` or `ClientKind` outside its constants, a nil `Signaler`, a `Conn` id that is already
  joined, or a `Participant` that belongs to another `User`; `StartShare` with an empty share id, an id of a share
  that exists, or an unknown preset; `UpdateShare` with an unknown preset; an `UpdateSubscriptions` item with an
  unknown quality (a per-item error); `Probe` with an empty user or a transport outside the three (README S76;
  04's endpoint checks the transport before it calls). These are bugs in the hub, sfuplane or the wiring, so they
  get no code of their own: the log message names the call. (A Pion failure in `Probe`, a PC or an answer it can't
  make, is the ordinary retryable `sfu.internal`.)
- **A method of a later slice** ("interfaces first", README §4), until README S76: the whole §6.1 API was declared
  from S29 on, and a method whose slice hadn't landed returned an `*Error{sfu.internal}` whose `Unwrap` was the
  sentinel `ErrNotImplemented`; the message named the method and its README slice. After S29 these were
  `RestartICE`, `ResetPC` and `ClosePC` (S57), `SetDecodeCaps` (S69) and `Probe` (S76), and `Resync()`, which has
  no error to return, only logged at debug. S76 built `Probe`, the last of them, and took out `Error.Unwrap` and its
  `cause` field; the group 10 integration took out the sentinel with the wiring's log handler that had passed these
  errors on at debug level (04 §6.6).

sfuplane needs no special case for either: both are `sfu.internal`, which it maps to 01's `internal` (01 §15.4).
So until its slice landed, a call that reached one of these methods was answered `internal` on the wire: a
client's `pc.close` and `pc.restart` until S57, and its `caps.update` until S69. No signaling message reaches
`Probe`: its one caller is 04's connection-test endpoint, which README S80 wired (04 §7.7: `sfu.bad_sdp` is 400
`bad_sdp`, `sfu.transport_disabled` 409, `sfu.probe_limit` 429 and `sfu.closed` 503 `not_ready`; any other error,
`sfu.internal` among them, a 500).

01 owns the error envelope (`error{code, retryable, scope}`); `sfuplane` maps these codes to 01's catalog (01 §15.4)
and uses `Share` and `RetryAfter` when it builds the wire error. A §8.4 violation changes nothing, so a share whose
video offers no H.264 stays as it was: the hub ends it itself (`sfu.no_h264` → `codec_not_supported` with scope
share, then `StopShare(id, stopped)`).

**A share that ends while its pub offer is in flight** (for example `share.stop` crossing a re-offer) is not an error
(01 §9 rule 4 and §15.4 win over an earlier draft of this section): the hub drops that share's TrackRefs before
`HandleOffer`, and the server still answers. The SFU answers every sending m-section that has no binding, or whose
binding names a share that isn't a `pending`/`live`/`stalled` share of this Conn, with `a=inactive` and ignores it
(§8.4). There is no `sfu.unknown_track` for that case; the code remains only for the malformed bindings in the table.

### 6.4 How signal drives it (wire names are 01's)

01 owns every message name and field. 01's `sfuplane` (01 §15.4) is the only translator; this table is the short
version for SFU implementers:

| Wire (01) | `sfuplane` calls | SFU does |
|---|---|---|
| `hello.caps.decode` (`h264/<4 hex>` CodecKeys), `caps.update` | `SFU.Join(JoinParams{Decode})`, `Conn.SetDecodeCaps` | codec policy (§8.3), caps-driven video (§8.5) |
| `share.start {kind, preset, audio, ref}` → `ok ShareParams` | `Conn.StartShare({ID: hub's shareId, Preset, Audio, Source: kind})` | creates the Share (`pending`), returns encodings and codec (§8.6) |
| `share.update {shareId, preset}` | `Conn.UpdateShare` | new `ShareParams` |
| `share.stop`, `pc.close{pub}`, a pub offer without a share's tracks (any gen; only shares an earlier pub offer bound; the hub calls `StopShare` before `HandleOffer`) | `Conn.StopShare(id, stopped)` (the hub decides) | removes the Share |
| `pc.offer {pc: pub, gen, neg, sdp, tracks}` → `pc.answer` | `Conn.HandleOffer(PCPub, gen, neg, sdp, tracks)` → answer | §8.4 |
| `pc.offer {pc: sub, …}` ← | `Signaler.SendOffer(PCSub, gen, neg, sdp, tracks)` | §5.3 |
| `pc.answer {pc: sub, gen, neg, sdp}` | `Conn.HandleAnswer(PCSub, gen, neg, sdp)` | bind, DTLS gate, PLI, media |
| `pc.ice {pc, gen, candidate}` | `Conn.AddICECandidate(pc, gen, init)` | filtered (§7.3), buffered before the remote description |
| `pc.restart {pc: sub, gen, mode: ice}` / `{mode: rebuild}` | `Conn.RestartICE(PCSub, gen)` / `Conn.ResetPC(PCSub, gen)` | ICE-restart offer / new sub PC `gen + 1` (a closed sub PC is rebuilt either way) |
| `pc.close {pc, gen}` | `Conn.ClosePC(pc, gen)` (after ending the Conn's shares for `pub`) | closes that PC |
| `pc.restart {pc: pub, gen, mode: rebuild}` ← | from `PCStateEvent{PCPub, Gen, failed}` (the pub PC failed, or a new one missed its 10 s handshake) | the client rebuilds and offers `gen + 1` |
| `subscribe.update {subs: [{shareId, video, audio}]}` | `Conn.UpdateSubscriptions` | DownTracks, one debounced offer |
| `subscribe.status` ← | from `SubscriptionStateEvent` | §10 |
| `quality.hint {shareId, reason, codec?, encodings?, maxBitrate?}` ← | codec hints from `CodecPolicyEvent` (only `codec`, no `encodings`); viewer and admin hints from `QualityHintEvent` (the SFU's full `encodings`) | §8.3, §11 |
| `stats` ← (while `stats.watch`) | from `Conn.Stats()` | §13 |
| `room.state` shares ← | built by the hub from its own share records plus `RoomEvents.ShareUpdated` | §5.3 |

### 6.5 Reconnect and failure cases

| Case | Client (05) | Signal (01) | SFU |
|---|---|---|---|
| WebSocket drops, media fine | reconnect with jittered backoff, `hello{resumeToken}` | same session → `Conn.Resync()` | re-sends an outstanding sub offer (same `gen`/`neg`), current `SubscriptionStateEvent`s, one `CodecPolicyEvent` per own share, the last `QualityHintEvent` per share (if one was sent), rebuilt from the current cap, pause state and preset (§6.2); ICE-restarts a sub PC that isn't connected, or builds a new one if it was closed |
| WebSocket gone for 30 s | – | `Conn.StopShare(id, disconnected)` per share, then `Conn.Close` | removes its shares and DownTracks, closes PCs |
| ICE `disconnected` ≥3 s on sub | `pc.restart{pc: sub, gen, mode: ice}` | `Conn.RestartICE(PCSub, gen)` | ICE-restart offer, same `gen`, next `neg` (after a pending answer if one is outstanding). Nothing new if an ICE restart for this `gen` is queued or was offered less than 5 s ago with no `connected` or `failed` since, e.g. from `Resync()` on the same resume (§5.3); the client counts that offer (new `ice-ufrag`) as its restart. No server timer: the client rebuilds if it isn't connected 15 s after that offer (01 §10.4) |
| ICE `disconnected` ≥3 s on pub | re-offer with fresh ICE credentials, same `gen` | `Conn.HandleOffer(PCPub, gen, neg+1, …)` | normal answer. Pion restarts ICE; SSRCs stay |
| ICE `failed` on sub | `pc.restart{pc: sub, gen, mode: rebuild}`, then answers the new offer | `Conn.ResetPC(PCSub, gen)` | closes the PC, builds a new one (`gen + 1`) with all subscriptions, offers |
| ICE `failed` on pub | new PC, offer with `gen + 1` and the same `shareId`s in `tracks` | `Conn.HandleOffer(PCPub, gen+1, 1, …)` | old layers end → shares `stalled` → new tracks attach, PC `connected`, keyframe → `live`. Viewers don't renegotiate |
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
- reads `Transport.Advertised` to label a selected candidate pair: match the local candidate (protocol, address:port
  after rewrite) to an entry and use its `Via` (`udp` | `tcp443` | `tcp7882`). A TCP pair with no match gets "" and a
  debug log. Port numbers are never hard-coded. Stats, `Snapshot` and `Metrics` all use this label (§13);
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

### 7.3 SettingEngine (shared by the pub and sub APIs; the probe APIs differ in network types, muxes, ICE timeouts and checks per pair)

| Setting | Value | Why |
|---|---|---|
| `Transport.Apply(se)` (04) | UDP/TCP muxes, network types, interface and IP filters, rewrite rules, loopback (dev), mDNS disabled | §7.1 |
| `DisableActiveTCP(true)` | | the server never dials out |
| `SetLite(false)` | full ICE | S4 tested full ICE; no reason to change |
| Remote-candidate filter (in `AddICECandidate`, and on the candidates inside every remote SDP: pub offers, sub answers and probe offers, before `SetRemoteDescription`) | drop loopback and the server's own advertised addresses unless `Transport.IncludeLoopback` (04's `network.include_loopback`); drop unparsable candidates, host names (mDNS or not), link-local, multicast, broadcast, unspecified (`0.0.0.0/8`, `::`) and IPv4-compatible IPv6 (`::/96`) addresses always; drop RFC 1918, CGNAT, ULA and site-local (`fec0::/10`) addresses unless any `Transport.Advertised[].LAN` is true (the server itself advertises a private address, 04's LAN/Append case). IPv4-mapped and NAT64 (`64:ff9b::/96`) addresses are judged by the IPv4 address inside. Pion's `SetRemoteIPFilter` is not used: Pion also applies it to prflx candidates, which the server must keep learning | 01 §17: a client must not make the server probe internal networks. Client addresses are learned as prflx from incoming checks anyway |
| `SetICETimeouts(5 s, 15 s, 2 s)` | disconnected, failed, keepalive | the client drives restarts after 3 s; the server only cleans up |
| `SetICEMaxBindingRequests` | 75 on the pub and sub APIs: the failed timeout ÷ Pion's 200 ms check interval, at least Pion's 7. Pion's 7 on the probe APIs, which only ever answer a browser's checks | a pair is checked for as long as the client waits for an ICE restart; Pion's 7 give it up after about 1.5 s. A probe gains nothing from more, and its offer's candidates would get more checks. Below |
| `SetPrflxAcceptanceMinWait(300 ms)`, `SetSrflxAcceptanceMinWait(0)`, `SetHostAcceptanceMinWait(0)` | | when controlling (sub PC), browser pairs are prflx. Pion's default 1 s wait delays the first frame. 300 ms lets a UDP pair win over TCP |
| `SetDTLSConnectContextMaker` | 10 s timeout | plan guard |
| `SetSRTPProtectionProfiles` | `SRTP_AEAD_AES_128_GCM`, `SRTP_AES128_CM_HMAC_SHA1_80` | cheaper per-packet encryption |
| receive MTU | Pion default (1460) | |
| `DisableCloseByDTLS` | not set: Pion's default, which closes a PC when the peer's DTLS sends `close_notify` | decided in README S57 (§5.3). It works both ways: when the SFU closes a sub PC itself (a codec rebuild, §8.5), a client on Pion's defaults sees its own PC go `closed`, which a browser's never does. So a Go client (the recovery of 01's itest harness, whose viewer is a `sfutest.Viewer`) treats a sub PC closed under it as `disconnected`, not `failed`: the new `gen`'s offer is on its way, and a rebuild request for the old `gen` would be one the client must not send (01 §10.4, README S74) |

**Checks per candidate pair** (README F10, S74-R1). While it connects, Pion's ICE agent sends one binding request
every 200 ms on each pair that hasn't answered (webrtc's `SettingEngine` can't change that pace), and gives a pair up
after `MaxBindingRequests` + 1 of them. A controlling agent never checks a pair again once it has given it up: it
still answers the client's checks, but it nominates only a pair that its own check got through. The SFU controls
every sub PC (it offers, and browsers are full ICE agents). With Pion's 7, a viewer whose network was still away when
it sent `pc.restart{sub, ice}` (01 §11.5 B: 3 s after its ICE noticed) had each pair of that restart given up about
1.5 s after its answer, and got its media back only through the rebuild 15 s after the restart's offer (01 §10.4).
`maxBindingRequests` (`api.go`) gives a pair the checks of the failed timeout instead: 15 s, as long as the client
waits for the restart before it rebuilds. It is set in `settingEngine`, so the pub API gets it as well as the sub
API; `probeAPI` sets Pion's 7 back:
- A controlled agent doesn't need it, and that is what the SFU is on a pub or probe PC, which the client offers. It
  answers each of the client's checks with a triggered check of its own on any pair that hasn't succeeded, a
  given-up one included (Pion skips it only on a pair that has succeeded, once the agent has selected a pair), and
  that check's answer revives the pair: itest `TestUDPBlackHole` recovered a sharer's pub PC that way with Pion's 7,
  from an ICE restart that the sharer offered about 2 s before its network came back. On a pub PC the setting only
  lets the SFU's own checks of a silent pair go on longer, at the same pace and on the same pairs (at most 64 remote
  candidates per PC and `gen`, below), and they end when the agent fails at the latest: each pair of a candidate
  that the client names gets up to 76 checks over 15 s, where Pion's 7 sent 8.
- The role is the negotiation's, not the API's: Pion controls a pub PC whose offer says `a=ice-lite`
  (RFC 8445 §6.1.1), and switches roles after a 487 role conflict. One setting for both keeps those right too,
  and the pub and sub APIs keep one `SettingEngine`, as this section's heading says; the prflx wait above is a
  setting of the same kind, which only matters when the SFU controls.
- The probe APIs keep Pion's 7. A browser's probe offer has no candidates (§7.6): the SFU learns the browser's
  address as prflx from its checks and, as the controlled agent, answers each with a triggered check, so a probe
  gains nothing from more. Only a hand-made `a=ice-lite` probe offer would make the SFU control a probe PC. What more
  would cost: a probe offer's candidates are the client's (up to 64, at addresses of its choosing; README S76), and
  any signed-in user may post 12 probe offers a minute (04 §7.7). With the 40 that the probe's 8 s would give, each
  of those candidates would get 41 checks instead of 8, about five times the STUN that S76 bounded.

**The filter also counts** (README S57; the cap is §12's 64 per PC and `gen`). Pion's ICE agent keeps every remote
candidate that equals none it already has, pairs it with each local candidate and sends its checks to it, and has
no cap of its own. So every candidate that reaches Pion goes through one count per PC and `gen`
(`remoteCandidates` in `conn.go`), wherever it comes from:
- **What is counted is a candidate as Pion keeps it, not a transport address.** For each candidate it keeps, the
  filter returns a `candidateKey` with exactly the fields `ice.Candidate.Equal` compares: network type, address
  (as the client spelled it), port, TCP type, candidate type and related address. Two candidates on one address
  that differ in type or related address are two of the 64, as they are two to Pion; two with one key are one.
- **Trickled candidates and those inside a remote description share the count.** A pub offer's and a sub answer's
  candidates are judged before `SetRemoteDescription` (`limitSDP`): what the filter drops and what is past the cap
  is taken out of the SDP. They count only once Pion has taken the description, so a description that is refused
  admits nothing.
- **A candidate that was admitted costs nothing when it comes again**: the candidates a client repeats in every
  re-offer, or sends again after an ICE restart, don't use the 64 up. (After an ICE restart Pion has forgotten
  the ones it had, so an admitted candidate is given to it again.)
- **Buffered candidates count like applied ones.** One that arrives before its PC has a remote description waits
  and is applied when the description is set.
- **The pub PC's candidates are its own.** The client owns the pub `gen`, so it can trickle for a `gen` whose
  offer the SFU hasn't accepted yet. Those wait in a buffer of their own (`Conn.pubAhead`, for the newest such
  `gen`, 64 too) and become that `gen`'s when its offer is accepted. A stray candidate for a later `gen` can
  therefore neither empty the current PC's count nor keep that PC's own trickled candidates out. Candidates for
  an older `gen`, for a pub `gen` whose PC has closed, and for any sub `gen` but the current, open one are
  ignored.

Everything this drops is dropped without an error and logged at debug (a trickled candidate with its reason,
`limit` for the cap; those of a description as a count). A probe offer of §7.6 goes through the filter and a
count of its own (README S76, `filterProbeOffer`): its one remote description is all a probe PC ever gets, so the
count starts and ends with it, and past the first 64 the candidates are dropped like a PC's. A browser's probe
offer has none, but a 64 KiB one could carry some 1,400, and the probe PC, the controlled agent, sends its checks
to every pair it has.

Peer-reflexive candidates, which Pion learns from the checks that reach it, never pass through this filter (it
must keep learning them, the table's row above). Their source addresses are bounded below Pion, in 04's netx: 64
per local ICE ufrag (§12, 04 §7.8; README F06, from the M1 security review).

### 7.4 ICE-TCP on 443

04's `netx.PortMux` hands non-TLS connections (first byte `0x00`–`0x02`) to an `ice.TCPMuxDefault` inside
`netx.Transport` (04 §7.2–§7.3), with a 10 s first-byte and first-STUN timeout and per-IP limits. The SFU sees only
the resulting `ice.TCPMux`. There is no 443 listener in `tls.mode=off`.

### 7.5 Diagnostics

04's doctor and dashboard read `Advertised` and the effective socket buffers (`RcvBuf`, `SndBuf`) from
`netx.Transport` (04 §7.3, §13.2). The SFU adds only `Ready()` (APIs built) for `/readyz`, `Snapshot()` and
`Metrics()` (§13).

### 7.6 Connection-test probe (M1; the REST endpoint is 04 §7.7, the UI 05 §14.2)

The setup wizard's connection test (and "Test my connection" for any signed-in user) runs real ICE checks per
transport. The SFU provides the probe PCs:

```go
type ProbeTransport string // "udp" | "tcp443" | "tcp7882" (each unavailable when its mux is nil)
type ProbeResult struct {
	Transport ProbeTransport
	Connected bool
	Selected  string        // "udp 203.0.113.5:7882" (server side of the nominated pair)
	RTT       time.Duration // from the candidate-pair stats
	Err       string        // "timeout" | "closed"
}
// The Err values of a ProbeResult that didn't connect (README S76).
const (
	ProbeErrTimeout = "timeout" // its life ended first, or ICE (8 s without a working pair) or DTLS failed
	ProbeErrClosed  = "closed"  // the client closed its side, a newer probe of the user's replaced it, or the SFU closed
)
// offerSDP: a browser offer with one data channel (m=application) and no media. The answer is complete (no trickle)
// and holds only candidates of the requested transport. Browser candidates are not needed: the browser's checks
// reach the public address and the server learns prflx.
func (s *SFU) Probe(ctx context.Context, user UserID, t ProbeTransport, offerSDP string) (answerSDP string, res <-chan ProbeResult, err error)
```

- Up to three extra `webrtc.API`s built at startup, one per transport whose mux is not nil: UDP
  (`Transport.UDPMux`), TCP 443 (`Transport.TCPMux443`) and TCP 7882 (`Transport.TCPMux7882`). Each has an empty
  MediaEngine and SCTP on. Its SettingEngine is built in these steps: `Transport.Apply(se)`; then
  `se.SetNetworkTypes` with the udp or tcp subset of `Transport.NetworkTypes` (IP family: next bullet); for
  tcp443 and tcp7882 also `se.SetICETCPMux(Transport.TCPMux443)` or `se.SetICETCPMux(Transport.TCPMux7882)`. Each
  probe keeps only its own transport's mux: a TCP probe gets no UDP mux and the UDP probe no TCP mux, because Pion
  (ice v4.4) gathers a host candidate on every address of a UDP mux whatever the network types. For the same reason
  an IPv4-only UDP probe (next bullet) gets a view of `Transport.UDPMux` that lists only its IPv4 sockets. 04
  guarantees that `Apply` only calls SettingEngine setters, so a later setter call wins. TCP candidates come from
  Pion's interface scan instead, which never returns `::1`; so on a dual-stack loopback Transport (development) IPv6
  shows on UDP only, and `Transport.Advertised` lists `::1` for UDP only (04 §7.6).
- IP family: when 04's `Public.V4` is set (the SFU sees it as a non-LAN IPv4 entry in `Transport.Advertised`), the
  probe APIs use only `udp4`/`tcp4`, so a pass reflects the path most friends use. IPv6 is used only on IPv6-only
  servers.
- `Probe` returns `sfu.transport_disabled` whenever the mux for the requested transport (`UDPMux`, `TCPMux443`,
  `TCPMux7882`) is nil, not only for tcp443: `udp` with `listen.ice_udp` = "", `tcp443` in `tls.mode=off`, `tcp7882`
  with `listen.ice_tcp` = "". 04 answers 409 `transport_disabled` for all three.
- The probe PC **echoes every data-channel message** back unchanged, so the browser measures RTT itself (05 §14.2).
- ICE timeouts: disconnected 3 s, failed 8 s; DTLS 10 s. A probe PC lives at most 20 s, or until the channel closes.
  `res` gets one value, when it connects (RTT sampled 1 s later) or times out. `res` has capacity 1 and the SFU never
  blocks on it; callers may ignore it (04's wiring does). It is used by integration test 18.
- Guards: one probe per (user, transport): a new probe closes that user's previous probe of the same transport. 20 per
  server (`sfu.probe_limit`, retryable; 04 answers 429). 04 adds 12 requests per minute per user.
- Access: any signed-in user (04). Only admins see provider fix text (05).

**As README S76 built it** (`probe.go`, the probe APIs in `api.go`), where the list above leaves a choice:
- **Order of the checks**: an empty user or an unknown transport is a contract violation (`sfu.internal`, not
  retryable, §6.3); then `Ready()` (`sfu.closed` after `Close`, whatever else is wrong); `sfu.transport_disabled`;
  the offer guard; the candidates; the slot (`sfu.probe_limit`, or `sfu.closed` when `Close` came meanwhile). A
  probe that a newer one of the same user and transport replaced before `Probe` returned gets `sfu.closed`.
- **The offer guard** is the SFU's own: at most 64 KiB (`maxProbeOfferBytes`, the limit of a pub offer, §12; a
  browser's probe offer is about 1 KB), parsable, and exactly one m-section, which must be `m=application`. Anything
  else, and an offer that Pion's `SetRemoteDescription` refuses, is `sfu.bad_sdp` (§6.3; 04 answers 400 `bad_sdp`).
- **The offer's candidates** go through the remote-candidate filter of §7.3 and a cap of 64 counted as for a PC
  (§7.3, §12); the excess is dropped and counted at debug. A browser sends none, so the cap costs nothing, and it
  keeps one request from making the probe PC, the controlled agent, check some 1,400 pairs.
- **One data channel per probe PC.** The first channel is served; any further one is closed as it opens, because
  each channel costs Pion a goroutine for the probe's life. The probe ends when its channel closes.
- **No echo while more than 1 MiB of echoes waits to go out** (`maxProbeEchoBuffered`, the channel's
  `BufferedAmount`): Pion queues data-channel sends without a bound, so a client that sends without reading would
  otherwise have the server hold its echoes for the probe's 20 s. The browser's five small pings are far below it.
  Text comes back as text and binary as binary.
- **The result**: `res` gets one value. For a probe that connected, 1 s after it connected (`Selected` and `RTT` from
  the selected candidate pair then), or when the probe ends sooner, with what is readable then (`RTT` 0 when unknown,
  `Selected` "" once the PC is closed). For one that didn't, `Err` says why: `ProbeErrTimeout` when its life ended, or
  its ICE (8 s without a working pair) or DTLS failed first; `ProbeErrClosed` when the client closed its side (its
  data channel, or its PeerConnection with a DTLS `close_notify`), a newer probe of the same user and transport
  replaced it, or `SFU.Close` stopped it. `Selected` is the server's candidate with its transport label and its
  address after the rewrite rules (`udp 203.0.113.5:7882`); a TCP candidate that no advertised address matches is
  labeled `tcp`. One info line per probe that was handed out says how it ended, without addresses (§13).
- **`SFU.Close`** stops every probe, replaced ones included, and waits for them within its 1 s (§5.4); a probe
  still closing its PC then is counted in one warning.
- **What is exported beyond §6 and §18**: `ProbeErrTimeout` and `ProbeErrClosed`; the test helpers of
  `export_test.go` (`Timings.ProbeLife`, `MaxProbes`, `SFU.Probes`, `SFU.HasProbe`) are for this package's tests.

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
| Interceptors, in this order | the PC's own NACK filter (`pubnack.go`, below), `nack.NewGeneratorInterceptor(GeneratorSize(2048), GeneratorInterval(50 ms), GeneratorMaxNacksPerPacket(10))`, `report.NewReceiverInterceptor(ReceiverInterval(1 s))`, `twcc.NewSenderInterceptor()` | **none**: no NACK responder (the shared cache serves NACKs), no SR generator (SRs are forwarded), no stats interceptor |
| Not registered | NACK responder, report sender, stats interceptor (the SFU counts itself) | |

The pub registry is built by hand, not with `RegisterDefaultInterceptors`. S4's sub API used `ConfigureNack` and
`ConfigureRTCPReports`; M1 drops both.

**The publish side as README S63 built it** (`codec.go` `pubInterceptors`, `api.go` `pubAPI`, `pubnack.go`):
- **The chain is `[NACK filter, NACK generator, receiver reports, twcc]`.** Pion hands each interceptor the RTCP
  writer of those registered before it, so the generator writes its NACKs through the filter. The filter takes
  out of each NACK the sequence numbers whose packets the cache of the track's Layer already holds, however they
  came, and drops a NACK with nothing left (§9.5). Every other RTCP packet, the SFU's own PLIs included, passes
  unchanged, and so does a NACK about an SSRC the filter doesn't know yet. `TestInterceptors` holds the order,
  `TestPubNACKFilter` the filter.
- **The generator asks for one packet at most 10 times** (`pubNACKTries`; `GeneratorMaxNacksPerPacket(10)`, every
  50 ms, so for half a second). The filter ends the requests for a packet that came back; the cap bounds those for
  one that never comes. Without it Pion's generator asks for as long as its window of 2048 holds the sequence
  number: some 45 times at 900 packets a second, over a thousand times on a still screen. Ten requests that
  brought nothing back are of no use any more (the viewers have asked for a keyframe), and fewer would cost the
  repair from publishers far away whose first retransmission was lost too.
- **One `webrtc.API` per pub PC.** The filter has to know the PC's tracks (by SSRC, a copy-on-write table that the
  Conn's actor keeps), and Pion builds a PC's interceptors from its API's registry while it creates the PC and
  names them only by a PC id made of the time, which two PCs created at once can share. So each pub PC gets an API
  of its own (`apis.pubAPI`): the pub MediaEngine, a registry with that PC's filter, and the one pub
  `SettingEngine` that `New` builds (§7.3), shared by all of them. `New` builds one such API without a filter, so
  that a pub engine that can't be built fails `New` and not every offer. The sub API and the probe APIs stay one
  per SFU (§7.6).

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
- On a change: `RoomEvents.CodecPolicyChanged`, and one `CodecPolicyEvent{Share, Profile}` per share, sent to the
  Conn that publishes it. `sfuplane` turns each into `quality.hint{shareId, reason: codec, codec}` without
  `encodings`, because a profile switch doesn't change encodings (01 §15.4). A browser sharer re-offers with that
  profile first in `setCodecPreferences` when the codec differs from the one it last applied (05). A native sender
  (M2) re-offers and sends an IDR in the new profile. New shares get the current policy in `ShareParams.Profile`.
- A new viewer's `bestPT` fails for a share while the publisher still sends High: its video DownTrack forwards
  nothing and reports `SubscriptionStateEvent{Forwarded: off, Reason: codec_mismatch}`. It recovers on the first
  keyframe in a compatible profile (the PT change is handled like a layer switch, §9.4).

### 8.4 Publish answer

`HandleOffer(PCPub)` runs these steps in the actor:
1. **Validate** the parsed offer (`sdpcheck.go`), before anything is applied: ≤64 KiB, ≤8 m-lines, no
   `m=application`. A sending m-line (`sendrecv` or `sendonly`) carries a share when its mid is bound in `tracks` to a
   share of this Conn in `pending`, `live` or `stalled`. A sending m-line with no binding (the hub drops the TrackRefs
   of a share that ended while the offer was in flight, 01 §9 rule 4), or bound to any other share (the same race
   when the caller kept the binding, or another Conn's share), carries no share: it is not an error, skips every
   check below (the kind check too), gets `a=inactive` in step 5, and the SFU attaches nothing that arrives on it.
   `tracks` binding one mid twice, a binding of this Conn's share whose kind isn't its m-line's, or a share's second
   video or audio m-line is `sfu.unknown_track` (per share: at most one video and one audio m-line).
   Video offers H.264 packetization-mode=1. Rids ⊆ {f,q}, at most 2, or none (one `f` layer). A violation returns its
   §6.3 code (with `Error.Share` set when the failing m-section maps to a share) and changes nothing. Before all this,
   a lower `gen` or a lower `neg` in the current `gen` returns `sfu.stale_offer`, and a repeated `neg` gets the stored
   answer.
2. Create the pub PC if needed, or replace it when `gen` is higher than the current one (PC guards, §12). Store the
   mid → share binding for `OnTrack`. Then `SetRemoteDescription(offer)` and flush buffered candidates.
3. **Filter codecs** on the transceiver of each video m-line that carries a share: `SetCodecPreferences` keeps, in
   the order of the offer being answered, only the profiles the room policy allows. `high` allows all five; `cb`
   allows `42e0` and `4200`. Plus their RTX. The browser then has to encode an allowed profile. Pion honours a
   transceiver's preferences in its answers (README S69 verified it: integration 12 and `TestPubAnswerCodecFilter`),
   so the fallback of stripping the other PTs from the offer is not needed. As S69 built it (`Conn.filterCodecs`):
   - **The filter is set on every offer**, a re-offer in the same `gen` included. Pion takes a transceiver's codecs
     from the remote description only when it creates the transceiver, and from then on answers with the
     preferences last set on it, so a filter set while the policy was `cb` would otherwise still hold once it is
     `high` again.
   - **It chooses from the first offer's list.** For the same reason the codecs Pion matched in the offer that
     created an m-line's transceiver are kept by mid (`pubPC.offered`) and are what every later filter chooses
     from, put in the current offer's order by payload type (`inOfferOrder`); a codec the current offer doesn't
     list is left out. So a sharer that re-offers with the hinted profile first gets it first in the answer
     (01 §8.10).
   - **An offer with no profile the policy allows** (a native sharer that can encode only High while the room is
     `cb`) is answered with every profile it has, as without a policy, and that is logged at info. A viewer that
     can't decode the stream then hears `codec_mismatch` (§8.3), which is better than a share without video for
     everybody. An offer in which Pion matched no profile at all is not filtered.
4. `CreateAnswer`, `SetLocalDescription(answer)`, wait for gathering (≤2 s).
5. **Edit the copy sent to the client** (not the local description; these attributes only tell the remote side what
   to do, so Pion's local description can keep its own):
   - **Opus per share**: Pion answers with the offerer's Opus fmtp, not the engine's (Chrome offers
     `minptime=10;useinbandfec=1`), so on each audio m-line that carries a share, set `stereo=1;sprop-stereo=1` (a
     sender encodes stereo only when the receiver's SDP asks for it, S4) and `maxaveragebitrate` to its share's preset:
     Movie 256000, otherwise 128000. Other parameters keep their order; a missing fmtp line is added.
   - **No share**: each sending m-line that carries no share (step 1) gets `a=inactive` in place of Pion's `recvonly`
     (Pion v4.2 has no exported per-transceiver direction). The browser then stops sending on it; its transceiver
     stays usable for the client's next share, which the next offer binds.

   Then return the answer to the caller (`HandleOffer` is synchronous) and keep it as the stored last answer for a
   repeated `neg`.

### 8.5 Viewers without H.264 (fresh Firefox profile, S4)

- **Caps-driven**: a Conn with empty `DecodeCaps.H264` gets audio DownTracks only. Each subscription reports
  `Forwarded: off, Reason: decoder_unavailable`. 05 shows "Your browser is still getting its video decoder,
  retrying…" and polls `RTCRtpReceiver.getCapabilities('video')` every 5 s.
- **Recovery**: `SetDecodeCaps` going from no H.264 to some rebuilds the sub PC (close, new PC, all DownTracks, offer).
  A new Firefox PC picks up the newly installed OpenH264. Renegotiating the old PC may not, so it isn't relied on. It
  costs one audio gap of about 1 s, once per profile.
- **Server guard** (caps wrong or stale): an answer that refuses H.264 on a video m-line with a DownTrack doesn't fail
  the negotiation the way it did in S4, because `DownTrack.Bind` never fails. With video codecs but no usable H.264
  it returns the first negotiated one; for an m-line the viewer rejected (port 0), which Pion binds with an empty
  codec list, it returns a zero codec, which Pion accepts. Either binding is marked `unsupported`, and nothing is
  written through it. The subscription then reports `decoder_unavailable`, and the SFU retries with a sub PC rebuild
  every 20 s, at most 9 times (3 min, matching 05's "Reload the page" hint). After that the reason is
  `decoder_failed` until the next `SetDecodeCaps` that changes the caps. The fake-Firefox integration test pins this
  down (integration 13).
- **One retry loop, on the server** (plan: "the SFU must retry a subscription that failed on the codec"). Clients only
  poll their capabilities and send `caps.update`; they never send `pc.restart` for codecs (01 §11.7). On the wire all
  three internal reasons are `reason: codec` (01 §15.4).

**As README S69 built it** (`codecstate.go`; `DownTrack.Bind` in `downtrack.go`; `checkSubAnswer` in
`sdpcheck.go`).
- **What Bind could bind drives the guard.** Pion fixes a sub PC's video codecs with the first answer that has
  video in it, so a PC that took an answer refusing H.264 can carry no video any more, whatever later answers on it
  say, and the bindings are the truth of what the SFU can send. After Pion has taken an answer (`Bind` runs inside
  that `SetRemoteDescription`), a video DownTrack bound as unsupported on that sub PC marks the PC as one that
  refused video and starts the guard (`Conn.codecAnswered`). The guard ends when an answer of a sub PC that never
  refused binds video with H.264. `checkSubAnswer` still runs before `SetRemoteDescription` and finds the video
  m-lines without H.264 (by position, rejected ones included), but its result is only logged at debug.
- **No fallback is needed.** Pion binds a rejected m-line with an empty codec list and accepts the zero codec that
  `Bind` returns, so the design's fallback (remove those video senders before the answer is applied, and re-offer)
  was not built.
- **When `SetDecodeCaps` rebuilds.** The caps are compared as sets: the same profiles again, in whatever order, are
  a no-op, so a `caps.update` about something other than decoding starts no rebuild and leaves the guard as it is.
  Caps that change reset the guard (no retries made, not failed) and rebuild the sub PC when the viewer has H.264 now
  and either had none before or some subscription's video is held by the codec: by the decoders (`decoder_unavailable`,
  `decoder_failed`) or by a stream in a profile its sub PC negotiated no payload type for (`codec_mismatch`). That is
  01 §8.10's "retries codec-blocked subscriptions". Caps that lose H.264 rebuild nothing: the decoders' cap stops the
  video (§10.1), and new video DownTracks stay off the sub PC.
- **A sub PC without subscriptions is not rebuilt**, since there is nothing to offer: it is marked stale, and the
  Conn's next subscription rebuilds it instead of renegotiating it (an SFU rebuild, not counted, §12). A codec retry
  that comes due on a Conn without subscriptions does nothing.
- **A closed sub PC is never rebuilt for the codec**, neither by `SetDecodeCaps` nor by a retry. It stays closed until
  the client's next request (§5.3), and that request's successor counts toward the PC creation limit (§12) and
  carries the video as the caps stand then. Otherwise a client could close its sub PC and toggle its caps for PCs
  that nobody counts (`TestDecoderCapsOfClosedSubPC`).
- **A codec rebuild that fails** reaches the client as an `ErrorEvent` with scope `pc.sub`, like any rebuild that no
  call returns.
- **After `decoder_failed`, only a caps change or a reload starts the retries again** (a known M1 limit, README §6
  "Decided after group 9" (4); 06 §9.5). Equal caps start nothing, and a client sends `caps.update` only when its
  capabilities change. So a Firefox whose first `hello` already listed H.264 (Firefox can list it before it can
  use it) and whose OpenH264 download takes longer than the 3 min of retries stays `decoder_failed` until a reload,
  which gives fresh caps and a fresh PC. 05's "Reload the page" hint appears at the same 3 min (05 §10.6).

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
- S29 already returns this table from `StartShare` and `UpdateShare` (`share.go`; §6.1), ahead of slice 7: `q` is
  never touched by the cap, and a cap at or above 8000 kbps changes nothing.
- Both encodings start `Active: true` in `StartShare`. Layer pausing (§11) later sends `quality.hint` with `f`
  inactive, and from then on `UpdateShare`'s and every hint's encodings carry the pause state (README S88: the
  paused layers inactive, the others active); `StartShare`, `UpdateShare` and the hints build their lists in one
  place (`presetEncodings`).
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
	stats   layerStats             // atomics: packets, bytes, lost, duplicates, too-late drops, EWMA bitrate and pps, fps
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
  an exact-size slice (one allocation per incoming packet, none per viewer), build `packet`, `cache.insert` (§9.2).
  If it returns `duplicate` or `tooLate` (a packet a capacity or more late), count it and stop: no fan-out.
  Otherwise, for each DownTrack in the share's list for that kind: if `dt.interest&slotBit != 0`, `dt.enqueue(p)`.
  Padding-only packets are enqueued as padding markers (no payload) to DownTracks currently forwarding this layer.
  The loop ends on a read error: the Layer detaches and the Share re-evaluates its state.
- Publisher RTX is unwrapped by Pion into the main track (repair stream), so recovered packets arrive late with their
  original seq. A recovered packet that fills an empty cache slot is cached and forwarded like any late packet. A copy
  of a packet already in the cache is a `duplicate` and is dropped. Chrome sends such copies on RTX as padding and
  for bandwidth probes (`kRtxRedundantPayloads`; periodic ALR probing is on for screen content), and Pion drops only
  the RTX packets without payload. Forwarding the copies would cost egress for every viewer and raise each viewer's
  received count in its RRs, which hides real loss from the allocator (§10.2).
- **RTCP loop**: simulcast video uses `recv.ReadSimulcastRTCP(rid)`, audio and single-layer video use
  `recv.ReadRTCP()` (S4 finding 2). This also keeps the pub interceptors (NACK filter and generator, RR, TWCC,
  §8.1) running. A `*rtcp.SenderReport` for this SSRC updates `lastSR` and enqueues an SR item to DownTracks
  forwarding this layer (`Layer.srNotice`, §9.3).
- **Keyframe start** (`h264.go`, kept from S4): the first NAL is an SPS (type 7), or a STAP-A (type 24) holds one. The
  STAP-A walk is bounds-checked and fuzzed.
- **SPS parser**: profile_idc, constraint flags, level and size (with cropping), for `LayerInfo` and the dashboard.
  It reuses the parser from `spikes/s2-mac-vt/h264.go` (our own code) and its tests.
- **Stats**, every 1 s from the ticker: bitrate and pps (2 s EWMA), FPS (distinct timestamps per second), loss (from
  seq gaps not filled within 1 s), `Active` = a packet within 2 s.
- **Interarrival jitter** (README S88, for the uplink policy of §11, `LayerUplinkStats.Jitter`): RFC 3550 §6.4.1's
  estimator in fixed point (×16, in ticks of the stream's RTP clock), updated by the RTP loop. Only a packet that is
  the newest of the stream when it arrives counts, and of those only one with a new timestamp (the first of a video
  frame) is a sample, against the last newest packet before it, as libwebrtc does (`receive_statistics_impl.cc`): a
  late or repaired packet would count its own delay, and the packets of one frame, which leave together, would add
  many samples near 0 that hide the frames' jitter (review S88-R3). Every newest packet still moves the reference
  on, so a frame is measured from the last packet of the one before. A packet more than 10 s after the last sample
  (`jitterGap`, a layer paused for a while) starts the estimate again from there, as the first packet does.

### 9.2 Packet cache (one per Layer)

- A ring indexed by `seq & mask` holding `*packet`, with each slot's seq for validation. Capacity is a power of two:
  `clamp(nextPow2(ceil(pps × 1 s)), floor, 8192)` with `floor` = 1024 for video layers and 128 for audio. That is 1024
  slots for an 8 Mbps full layer (about 900 pps) and for every preview layer, and 128 for audio (2.5 s at 50 pps).
- Why the video floor: a static screen runs at about 1 fps, so a pps-sized ring would be 128 slots. The first change
  (scroll, slide, game start) often begins with a frame of 150–400 KB, more than 128 packets, which would overwrite the
  start of its own burst before any viewer's NACK arrives (about one RTT). Each such miss makes the viewer send a PLI,
  and every viewer gets another large keyframe. The floor costs at most 1024 payloads (about 1.2 MB) per video
  layer, busy or idle.
- Resizing is checked each second: grow at once when the wanted size is more than the current one; shrink only after
  10 s at a quarter of the current size or less, never below `floor`. A resize copies the entries still in the window.
- `insert(p) cacheResult`: `tooLate` if `p.seq` is a capacity or more behind the highest seq (int16 arithmetic),
  `duplicate` if the slot already holds `p.seq` (§9.1: the slot is left as it is). Otherwise it overwrites the slot
  and returns `inserted`. Out-of-order and late (RTX) packets land in their empty slots. The Layer counts duplicates
  (`isshoni_sfu_ingress_duplicates_total`) and too-late drops separately.
- `get(seq, now) *packet`: nil if the slot holds another seq or `arrival` is older than 1.5 s before `now` (explicit
  for deterministic tests).
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

  **The reuse as README S52 built it** (`subpc.go`: `subPC.spares`, `takeSpare`). The sub PC keeps **its own list
  of spare transceivers** and lets Pion's `AddTrack` do the reuse only when that list says a spare is free. Both
  halves are needed:
  - *Why a list of its own.* Pion exports no "current direction" of a transceiver, and "inactive in our offer" is
    not enough: a new DownTrack on an m-section that the viewer doesn't know as inactive yet would look to it like
    the old track with another SSRC and msid, without the inactive step that makes a browser fire `track` again
    (§18). So a spare, a transceiver whose DownTrack left with `RemoveTrack`, remembers the `neg` of the first
    offer made since, and is **free** once the viewer has answered that offer, or a later one, with the m-section
    `inactive` (read from the answer as Pion reads it). `sfutest.DirectSignaler.WaitAnswered` lets a test wait
    for that moment.
  - *Why `AddTrack` all the same.* Its sender is the PeerConnection's own, so Pion binds the track to the codecs
    and header extensions **this viewer** negotiated, as it does on a new transceiver. The slice's first version
    made the sender itself (`NewRTPSender` on the subscribe API, then `SetSender`); that bound the track to
    everything the SFU offers, so from a viewer's second share on a viewer with a codec subset was sent payload
    types, an RTX stream and abs-send-time it never accepted. The review took it out
    (`TestSubTransceiverReuseBinding`).
  - *Keeping `AddTrack` in check.* It picks the transceiver itself (the oldest of the kind without a sender that
    the last answer didn't leave sending), and when it finds none it adds a **sendrecv** one, which a sub PC must
    never have. So it is called only while the list has a free spare of the DownTrack's kind, and what it took is
    checked against the list:

    | `AddTrack` took | Then |
    |---|---|
    | a free spare, now sendonly | the reuse: the m-section carries the new DownTrack under its msid and a new SSRC |
    | a spare that isn't free. Pion goes by the last answer alone, so it also takes one that was reused and lost its DownTrack again before the viewer answered, or one whose m-section the viewer refused | `RemoveTrack` puts it back as it was, and the DownTrack gets a new sendonly transceiver (`AddTransceiverFromTrack`): rare, and it costs one m-section (`TestSubSpareNotFreeYet`) |
    | a transceiver it added. That can't happen while "free" means what it says | the transceiver is stopped, which makes it inactive before any offer shows it, and kept as a spare; the spares Pion passed over are no longer free; an ERROR line |

  - Two cases of the text above are left out on purpose. A transceiver whose current direction is `recvonly` is not
    reused: on a sub PC that is an m-section the viewer answered as one it sends on, which no viewer may. And an
    answer that gives a spare's m-section a direction (against JSEP) does not free it, until an answer of that
    viewer puts it right (`TestSubSpareNotInactive`).
  - The result: a sub PC has as many m-sections as its Conn had subscriptions **at once**, at most, not as many as
    it ever had. Integration 10 runs five shares in a row through one viewer's two m-sections (§17). Nothing joins
    a sub PC that has closed.

  Clients must not rely on any of this: they map tracks by the offer's `tracks`, re-read on every offer
  (01 §9 rule 4).
- **Bind**: builds `ptFor` from `ctx.CodecParameters()` (§8.2), finds the RTX PTs (`apt=`), and gets `ctx.SSRC()`,
  `ctx.SSRCRetransmission()` and the abs-send-time extension ID from `ctx.HeaderExtensions()`. It returns the codec for
  the share's current profile (or the first negotiated codec of its kind) and never an error while one exists. It
  stores the binding and requests a keyframe if the sub PC is already connected.
- **Unbind**: `binding = nil`. The writer drops packets until the next Bind.
- **DTLS-ready gate** (S4 finding 1): the writer forwards only when `binding.pc.ready` is true. The sub PC sets it on
  `connected` and then requests a keyframe for every video DownTrack. A rebuilt sub PC is a new `subPC` with its own
  gate, so a stale "ready" can never leak.

  **The gate follows the PC, not only its first `connected`** (README S41, `subPC.follow`). Pion takes RTP written
  to a PC that has failed or closed the same silent way it does before DTLS is up: no error, nothing sent. So the
  actor moves the gate with every connection state it handles:

  | Sub PC state | Gate |
  |---|---|
  | `connected` | open; a keyframe request for every video DownTrack |
  | `disconnected` | unchanged: the selected pair is still there, packets may get through, and ICE often recovers |
  | `failed`, `closed`, and `connecting` again (an ICE restart, README S57) | closed |

  A gate that closes ends what the DownTrack was forwarding, and so does `Unbind`: the munger's epoch is over, the
  viewer leaves `ShareInfo.Viewers`, and the stream starts on a keyframe again when the gate reopens or the track
  is bound anew. While the gate is closed or there is no usable binding (none, or `unsupported`), the writer drops
  what it takes off its queue and never asks the munger for a place, so the first packet a viewer gets is always the
  keyframe start its first epoch begins with.
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
  - `PictureLossIndication`, `FullIntraRequest` → `share.requestKeyframe(current slot)` (video DownTracks only).
  - `TransportLayerNack` → §9.5, for audio as for video. One compound packet may have at most 256 of its sequence
    numbers looked at, across all its NACKs (§9.5).
  - `ReceiverEstimatedMaximumBitrate` → `allocator.onREMB(bitrate, now)`. The same REMB reaches several DownTracks;
    the allocator keeps the latest and compares each new value with it (§10.2), so a repeated copy changes nothing.
  - `ReceiverReport` blocks for our SSRC → `fractionLost` → DownTrack loss EWMA and `allocator.onLoss`. As README
    S79 built the first half: the last report's fraction lost, in percent and not averaged, is
    `DownTrackStats.LossPct` (§13).

  **Both go to the Conn's downlink allocator as README S84 built it** (`allocator.go`: `DownTrack.noteREMB`,
  `noteReport`; §10.2 has what the allocator makes of them):
  - **A REMB** counts on every DownTrack whose SSRC it names, video or audio, through the subscriber Conn's one
    allocator. A REMB of 0, NaN or above 100 Gbit/s is ignored before Pion's float32 is converted (past an int64
    the conversion differs between arm64 and amd64, and the allocator's products would overflow).
  - **A receiver report** goes to the allocator for a video DownTrack only. There is no EWMA: the DownTrack keeps
    what the last report of its stream said (SSRC, extended highest sequence number, cumulative loss, when; `rr`,
    under the allocator's lock), and hands the allocator what the next one adds, as the link lost it (§10.2): the
    packets expected and lost since the last report, less the own sequence numbers the writer passed over without
    sending them (`seqGaps`). The first report of a stream, one for another SSRC (a rebuilt binding) and one more
    than 2 s after the last only start the count again; one less than 200 ms after the last that counted changes
    nothing and is folded into the next, which covers both intervals (§12).
  - Either may need the allocator's decision before its next tick: a REMB that fell, or a report with 5 % loss or
    more, wakes the Conn's actor (`Conn.wakeAllocator`, one queued look at most). `LossPct` stays the last report's
    fraction lost, as above.
- **RTT** for the sub PC comes from the selected ICE candidate pair, sampled every 5 s. RR-based RTT is meaningless
  here because the forwarded SRs carry the publisher's NTP clock (§9.6). As README S63 built it (`subPC.sampleRTT`,
  `rtt` and `rttAt` atomics on the `subPC`): the RTCP readers of the PC's DownTracks take the sample when the
  viewer's RTCP arrives, before they act on it, whichever reader comes first after the 5 s (a compare-and-swap on
  `rttAt`; the first RTCP of a new sub PC takes one at once). It asks the sender's DTLS transport for its ICE
  transport's `GetSelectedCandidatePairStats` (`CurrentRoundTripTime`), only while the gate is open and DTLS is
  connected: Pion logs an error when a closed ICE agent is asked. A reader may wait for Pion's ICE agent a moment,
  a writer may not, which is why the readers do it. The NACK limits of §9.5 read the value, 0 while it is unknown.

**What README S41 built** (the media path with one layer; `layer.go`, `downtrack.go`, and the ticker in `sfu.go`).
The writer runs steps 1, 2, 3 and 5 of the loop above; a subscription forwards what it names (`high` is `f`, `low`
is `q`, `off` nothing, audio while `Audio` is set), and the fallbacks and events of §10.1 are slice 7's (README
S52 has added them since: §10.1, §6.2). Beyond the text above it settled five things:

- **`Share.notify`, a lock held across `RoomEvents` calls.** The media path is what makes a share `live`, on a
  layer's read loop, and signal may end the same share in the same moment. The ticker reports the first and
  `SFU.endShare` the second, and a per-share mutex keeps every `ShareUpdated` ahead of the `ShareEnded`. It is the
  one exception to the lock rules, written out in §5.4.
- **The gate follows the sub PC's state** (the table above).
- **A packet that Pion takes without sending it: `WriteRTP` returns `(0, nil)`.** Pion does that in two situations
  that have nothing in common, and the writer tells them apart by whether anything has left through the binding
  since its gate last opened:
  - *Nothing has left yet.* A sub PC reports `connected` a moment before its SRTP session is usable (S4 finding 1),
    and a sender's write path opens a moment after `Bind`. The viewer never saw the packet, which was a keyframe
    start, so the stream must not go on from it: the DownTrack **restarts** (the munger's epoch ends, the interest
    mask is the target's alone, the viewer is not counted) and starts again on the next keyframe, which the packets
    that now wait for it request (§9.7). A binding that never gets a packet out would repeat that at the layer's two
    keyframes a second for every viewer of the layer, so after two such starts in a row the DownTrack asks only once
    per 5 s until a packet leaves.
  - *In mid-stream.* The ICE transport can't send right now: no selected pair (a PC that has just failed, until its
    gate closes), or a full ICE-TCP write buffer (a slow viewer; 04's TCP muxes drop when their 4 MiB buffer is
    full, §5.4). That is an ordinary lost packet, counted and nothing more: the viewer sees the gap and sends a NACK
    or a PLI. Restarting here would turn every such packet into a keyframe for all viewers of the layer (a viewer
    whose sub PC had failed cost its publisher a PLI every 500 ms before this rule; `TestFailedViewerCostsNoKeyframes`
    pins it).

  A `WriteRTP` error (the sender stopped, the PC is closing) is counted; `Unbind` follows.
- **A profile the viewer has no payload type for** (`ptFor` has no entry): the packet is counted and dropped, and
  nobody is asked for a keyframe the viewer couldn't decode. If the layer forwarded now has moved to that profile,
  what the viewer was getting is over (`munger.unforwardable`): a flip back to the old profile then waits for a
  keyframe instead of going on across a gap that a NACK would fill with the other profile's packets. Reporting it
  (`codec_mismatch`, `DownTrack.mismatched`, §10.1) and fixing it through the room's codec policy are slice 10's
  (README S69 built both).
- **Done here, ahead of slice 8** (README S63, "repair and sync"), because the one-layer path needs it or gets it
  for nothing:
  - the **duplicate and too-late drop** at the cache insert (§9.1, §9.2), with both counters: a packet that isn't
    cached isn't forwarded, so Chrome's redundant RTX copies cost no egress from the first slice on;
  - **padding**: a padding-only packet is never cached and goes out as a marker, and the writer hands it to
    `munger.skipPadding`, so the viewer's sequence numbers have no gap where the publisher probed;
  - **abs-send-time** on every forwarded packet when the viewer negotiated it (step 3; the three bytes are reused,
    so forwarding allocates nothing);
  - the **latest sender report per layer** (`Layer.lastSR`, from the track's RTCP reader), which the munger's
    SR-aligned timestamp rule (§9.4) reads at a layer switch.

  Left then for slice 8, and built by README S63 since: the RTX queue and NACK handling (§9.5), forwarding
  translated SRs to the viewers (§9.6), and integration 4, 5, 6 and 19 (§17), which include the ones for padding
  and for the publisher's redundant RTX.

**What README S63 added to the DownTrack** (slice 8; `rtx.go`, `downtrack.go`, `layer.go`):
- **The second queue**, `rtxQueue chan rtxItem` (256), which the writer drains before it takes anything off
  `queue`: a viewer that waits for a repair can't show anything newer, so a retransmission that queued behind new
  media would add its delay to every frame after it. A full queue drops (counted, `rtxDrops`); the viewer asks
  again. An item carries the cached packet, the binding the NACK came through, and the own sequence number and
  timestamp it went out with; the payload stays the cache's.
- **`nack nackState`**, the RTCP reader's: the ring of what was retransmitted when, made with the first
  retransmission (most DownTracks never see a NACK), and the budget (§9.5). Its lock is a leaf.
- **Sender reports through the queue.** A Layer that gets a sender report queues one notice to its DownTracks
  (`Layer.srNotice`, a `packet` with `report` set, one for all of them, so a report allocates nothing however many
  viewers it reaches), and the writer sends the report on when it reaches the notice, in its place among the
  layer's packets (§9.6). A notice that finds the queue full is not counted as a drop: the next report comes in a
  second. The binding keeps the sub PC's PeerConnection to send reports on (`binding.rtcp`).
- **The writer's own counts** (`rtpWriteState`): packets and octets sent on the media SSRC since the binding
  began, which the sender reports carry (plain retransmissions count, RTX ones don't), and the RTX stream's own
  sequence number, random at the start (RFC 3550).
- **Counters** (`dtStats`, for README S79): `srs` (reports forwarded), and for repair `nackReceived`, `nackServed`,
  `nackMissed`, `rtxSkipped`, `rtxDrops`, `rtxPackets` and `rtxBytes` (§9.5 says what each counts). `packets`
  and `bytes` count forwarded media only, retransmissions not included.

`DownTrack` as built differs from the struct above in what later slices add (the pacer came with slice 11, README
S84, below). It has
`pc atomic.Pointer[subPC]` (set by the actor before Pion can call `Bind`, so the binding gets its gate),
`forwarding atomic.Bool` (the writer forwards a layer now: `ShareInfo.Viewers`; written only with `DownTrack.mu`
held, so a writer can't leave a paused viewer listed) and `stats` with packets, bytes, queue drops, packets
without a payload type, unsent packets, write errors, epochs started and the viewer's keyframe requests, and the
counters above. The RTCP reader handles PLI and FIR (video), NACK (both kinds) and, since README S79, the receiver
reports about its SSRC, for `LossPct`; since README S84 also REMB, and the receiver reports for the downlink
allocator (above). Since README S79 the counters that §13's totals need are tallies: each also counts into the
SFU's total of the same thing (`tally`, `counters.go`), so `Metrics` keeps what a DownTrack counted after it is gone;
and `stats` has `lossPct` and `bitrate`, a 2 s moving average of what the DownTrack sends, media and
retransmissions, which the ticker updates once a second (`DownTrack.tick`).

Since slice 7 (README S52) the request and the cap are the **Subscription's**, not the DownTrack's: `reqVideo`,
`reqAudio`, `capQ` and `capReason` belong to the subscriber's actor and need no lock. The DownTrack has one field
for them, `want` (under `DownTrack.mu`): what its subscription asks of it, the video quality under the cap, or for
audio "on" or "off". It chooses its layer for that among the layers the share has (§10.1), again whenever those
change. `sender` and `transceiver` are the actor's too: the m-section the DownTrack is on (the reuse above).

**What README S84 added to the DownTrack** (slice 11; `downtrack.go`, `pacer.go`, `allocator.go`, `rtx.go`):
- **`alloc`**, a pointer to the subscriber Conn's allocator (nil for a DownTrack a unit test builds without a
  Conn), which the RTCP reader hands REMBs and receiver reports (above), and which both queues tell of every drop:
  a full `queue` and a full `rtxQueue` add to `allocator.drops` as well as to the DownTrack's own counters (§10.2:
  more than 50 a second is congestion). `rr` is what the last receiver report of the video stream said, under the
  allocator's lock.
- **`gaps seqGaps`**: the own sequence numbers a video DownTrack's writer went past without sending them, a gap the
  publisher left that no repair filled in time, or packets its queue dropped. The viewer counts them as lost, but
  the link didn't lose them, so the next receiver report that covers them has them taken out (§10.2). The writer
  notes each own number it hands Pion (`noteSeq`, before pacing): a jump past the highest one adds the numbers in
  between, a late number that falls into such a gap takes one back. At most 64 entries are kept, the newest
  taking in the count past that; the lock is a leaf of its own, taken by the writer and by the reader, never inside
  the allocator's.
- **`pacer`**, the video writer's token bucket (§9.8), which only the writer touches. Step 4 of the writer loop is
  `DownTrack.pace` after the header is built and before abs-send-time is stamped, so the time a packet leaves is
  the one the viewer's estimator reads; a writer told to stop meanwhile (the DownTrack's stop, its Conn's close)
  drops the packet. Retransmissions go through the same bucket, first (§9.5, §9.8).

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
     `outTS = that + tsOff_old` and `tsOff = outTS − p.ts`. With `step = int32(outTS − lastTS)`: accepted if
     `0 < step ≤ (elapsed + 1 s)·clock`; **`outTS = lastTS + 1` if `−clock < step ≤ 0`** (README S63, below);
     otherwise, and after a pause of more than 8 hours (the wall clock's cap), rule 3 decides.
  3. *Wall clock* (the S4 rule): `outTS = lastTS + max(1, elapsed·clock)`.

  **The `lastTS + 1` rule** (README S63, `munger.srAligned`). A result at or less than a second behind `lastTS` is
  the common case of a switch, not a fault: a sharer's layers are encoded from the same captured frames, so the new
  layer's keyframe has the capture time of a frame of the old layer, and when that frame was forwarded first the
  keyframe belongs exactly where `lastTS` is. Two frames can't share a timestamp, so it gets the next tick, and the
  viewer's timeline stays the capture timeline to within one tick (11 µs at 90 kHz): no timestamp error while the
  reports are fresh (§9.6). The S12 rule sent such a switch to the wall clock, which put the keyframe, and
  everything the new layer sent after it, later by however long the keyframe took to arrive after that frame.
  Integration 5 asks for 5 ms; with this rule S63 measured flash to beep within 12 µs through ten switches, and
  the test also wants the forwarded reports' RTP time to advance with their NTP time within 200 µs, report to
  report, a tick at a switch included. A result a second or more behind means the two layers' clocks can't be
  trusted to be one, and the wall clock decides as before. The timestamp still only moves forward: the monotonic
  guard holds for every rule.
- **In-epoch packets**: `S = U + seqOff`, `T = ts + tsOff`. `lastS` and `lastTS` only move forward (int16/int32
  comparisons), so reordered packets keep their mapped seq.
- **Late packets** from before the current epoch's `startU`: look back through earlier epochs of the **same Layer**
  (padding epochs) and use the first with `startU ≤ U`. If a different layer is reached, drop the packet: its seq
  would collide with numbers already used.
- **Padding**: `skipPadding(p)` with `p.seq == lastU + 1` (in order) starts a new epoch with `seqOff − 1` (same layer,
  same `tsOff`), so the next media packet closes the gap. Out-of-order padding is ignored; the viewer may NACK that
  seq, it misses the cache, and nothing is lost.
- **Pause** (`off`): `active = false`. Nothing is forwarded. Resume follows the start rules (keyframe for video).
- **Lookup for NACK** `lookup(S, now) (epoch, ok)`: walk from the newest epoch back to the first with
  `int16(S − startS) ≥ 0`. Don't walk past an epoch that started more than 2 s ago (every earlier epoch ended before
  then); the current epoch always answers. `U = S − seqOff`.
- **SR translation**: `translateSR(layer, sr) (rtp uint32, ok)` succeeds only when `layer` is the current epoch's
  layer: `rtp = sr.rtp + tsOff`.
- Wraparound: all comparisons use int16 (seq) and int32 (ts) differences.
- **Result**: `process(p, now) (seq, ts, verdict)`. The verdict is `drop`; `waitKeyframe` (a target-slot packet
  while waiting for its keyframe: the DownTrack requests a throttled keyframe, §9.7); `forward`; or `newEpoch`
  (forwarded and started an epoch: the writer sends the new layer's translated SR, §9.6). The random first seq and ts
  are picked in `newMunger`.
- **Further rules** (S12, each tested): a newer packet of the current Layer with another profile ends forwarding
  until a keyframe, so a profile flip-back can't leave a seq gap that NACKs would fill with the wrong profile; a late
  packet is also dropped when its own seq would collide with a later epoch's start (a same-layer resume after a
  pause); packets more than 8192 behind are dropped, and an epoch's start is kept within 16384 of its newest packet
  so int16 comparisons stay valid; a run of in-order padding extends one padding epoch instead of adding one each; a
  wall-clock ts step is capped at 2^30 ticks; the monotonic ts guard also applies to a same-Layer resume, which then
  falls back to the wall clock.

### 9.5 NACK and RTX (video and audio)

For each lost own seq `S` in a `TransportLayerNack` (each NACK item names up to 17 seqs), in this order (README S63
built it so, `rtx.go` `handleNACK` and `serveNACKs`; the DownTrack's RTCP reader runs steps 1–6, its writer 7):
1. **At most 256 per compound packet** (`nackBatch`, the size of the RTX queue). One compound packet from a viewer
   may name thousands of sequence numbers; only the first 256, across all its NACKs, are looked at. More couldn't
   be sent anyway, and looking them up costs time under the DownTrack's lock, which a viewer must not be able to
   spend at will. The rest count as skipped, and the viewer asks again for what it still misses
   (`TestNACKBatch`).
2. **Look it up**: `e, ok := m.lookup(S, now)` (§9.4), then `U = S − e.seqOff` and `p := e.layer.cache.get(U, now)`.
3. **Asked too soon or too often**: skip it (counted as skipped) if `S` was retransmitted in the last
   `max(20 ms, rtt/2)` (`rtt` of §9.3, 0 while unknown), or 3 times already. The records are a ring of 1024 keyed
   by `S`, made with the DownTrack's first retransmission, and a record counts for 5 s after the retransmission it
   notes (`nackRecordAge`): own sequence numbers have 16 bits, and the number of a packet retransmitted three times
   belongs to another packet 65536 packets later (73 s at 900 packets a second), which must be repaired like any.
   Five seconds are more than the cache keeps a packet (1.5 s), so expiry never gets a packet a fourth sending
   (`TestNACKLimits`, `TestNACKRecordAge`). This check comes **before** the miss: a viewer that goes on asking for
   what it already got (Pion's own NACK generator does, for packets that arrived as RTX) is skipped, not counted
   as a miss once the cache has let go of the packet.
4. **A miss**: no epoch sent `S` (not sent yet, too old, or the lookup stopped at an epoch older than 2 s), or the
   cache no longer has the packet. Counted (`nackMissed`) and skipped; the viewer asks for a keyframe when it
   needs one.
5. **Retransmission budget**: a token bucket in bits whose rate is 25% of max(the forwarded layer's measured
   bitrate, what that layer may carry at most), at least 200 kbps, and whose depth is 1 s at that rate. The layer
   is the one of the DownTrack's newest epoch; a DownTrack forwards all of a layer or nothing, so the layer's own
   rate stands for "the DownTrack's forwarded bitrate". The maximum is the full layer's `MaxBitrate` after the
   admin cap (§8.6), the preview layer's 300 kbps, or for audio the preset's Opus bitrate (`AudioBitrate`); each is
   an atomic or a constant (`SFU.fullMaxBps`, set by `New` and `SetLimits`; `Share.audioBps`, set with the
   preset), so the NACK path takes no lock beyond its own three (`TestRTXRate`). For an 8 Mbps `f` that is 2 Mbps
   with a 250 KB depth, enough to repair the large first frame after an idle screen, when the measured rate is
   still low. The bucket starts full with the first NACK; a lower rate than last time also lowers the depth. A
   retransmission costs its payload; over budget, it is skipped and counted (`TestRTXBudget`).
6. **Queue it** on `rtxQueue` (256), which the writer sends before new media (§9.3). A full queue drops it
   (`rtxDrops`); the viewer asks again.
7. **The writer builds and sends it** (`retransmit`): `ts = p.ts + e.tsOff`, marker from `p`, abs-send-time of
   this sending when negotiated. With RTX negotiated for the viewer PT: SSRC = `rtxSSRC`, PT = RTX PT, the
   DownTrack's own RTX seq counter, payload = `S` (2 bytes, big endian) + `p.payload`, built in a buffer from a
   `sync.Pool`. Without RTX (always the case for audio): the original SSRC, PT and `S`. A retransmission asked for
   on another binding than the one the DownTrack has now (the sub PC was rebuilt meanwhile), or that the viewer can
   no longer get or decode (the gate closed, no PT for the profile), is skipped (counted).

**Retransmissions are paced, ahead of new media** (README S84, the open question that S63 left; §9.8). A video
retransmission goes through the DownTrack's token bucket like a forwarded packet, after its header and payload are
built and before abs-send-time is stamped, and the writer serves the RTX queue first, so a repair takes its tokens
before new media does and never waits behind a frame. A viewer whose NACKs ask for a large frame again gets it
spread like the frame itself, not as a second burst on top of the one that was lost. The budget of step 5 bounds how
much of the rate repair may use on average; the pacer bounds how fast any of it leaves. A retransmission whose
writer is told to stop while it waits is not sent and counts as skipped. Audio and its retransmissions are never
paced. A full RTX queue (step 6) is a queue drop for the downlink allocator too (§10.2).

The locks are taken one after the other, never inside each other: `DownTrack.mu` for the epochs (step 2), each
cache's own for the packet, and the NACK state's (a leaf) for the records and the budget (steps 3–5).

**What is counted** (`dtStats`, §9.3; README S79 exports them): every sequence number a NACK names is received
(`nackReceived`), and then either served (`nackServed`, when the write succeeded), missed (`nackMissed`, step 4),
skipped (`rtxSkipped`: beyond the 256 of step 1, asked too soon or too often, over budget, or dropped by the
writer in step 7), or dropped by the full RTX queue (`rtxDrops`). A write that fails or that Pion takes without
sending counts as `writeErrors` or `unsent`, like a forwarded packet. `rtxPackets` and `rtxBytes` count the
retransmissions written (with the 2 bytes of RTX's original sequence number); a plain retransmission also counts
in the packets and octets of the next sender report, since it is on the media SSRC, and an RTX one doesn't.

**The publish side needs more than Pion's generator** (README S63). Pion's NACK generator asks the publisher, and
the recovered packet reaches the cache and every forwarding DownTrack as a late packet (§9.1). But the generator
never learns of a packet that came back on the RTX stream: Pion's receiver reads that stream through an
interceptor chain of its own and hands the packets to the track past the generator. Left to itself it asked for
such a packet again at every tick, and a browser sends a copy for every request that comes a round trip or more
after its last one: some ten copies of each packet the publisher's link lost, all dropped as duplicates at the
cache. So each pub PC has the NACK filter of §8.1 in front of the generator, which stops the requests for a packet
as soon as its Layer's cache has it, and the generator asks for one packet at most 10 times, which bounds the
requests for a packet that never comes. In integration 19 the publisher now resends 20 packets for 20 lost ones
(200 before the filter).

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
  switches and pauses. The plan's 45 ms end-to-end target is checked by that Go test (integration 5, README S63; the
  plan puts the flash-plus-beep check in the Go integration tests) and by the exit session's survey ("nobody reports
  audio out of sync", 06 §12.3). No browser spec measures the A/V offset (05 §19.3).

### 9.7 Keyframe requests

- `Share.requestKeyframe(slot)`: finds the current Layer for the slot. It sends only if `now − lastPLI ≥ 500 ms`,
  using a CAS on `lastPLI`, so concurrent callers produce one PLI:
  `pubPC.WriteRTCP(rtcp.PictureLossIndication{SenderSSRC: pubPC's RTCP SSRC, MediaSSRC: layer.ssrc})`. Throttled calls
  are counted.
- Callers: a DownTrack binding on a connected PC; the sub PC reaching `connected` (every video DownTrack); the pub PC
  returning to `connected` (one request per video layer of its shares); a target change; a viewer's PLI or FIR (translated to PLI upstream); and, while a DownTrack waits for a keyframe, every packet
  of the target layer. The last one re-requests every 500 ms until the keyframe arrives, which heals a lost PLI.
- There is no keyframe cache or replay: P-frames after an old keyframe don't decode.

**As README S41 built it** (`Layer.requestKeyframe`; `Share.requestKeyframe(slot)` finds the layer and calls it):

- **A pending share asks for its own first keyframe.** A share goes `live` on its first SPS keyframe (§5.3), but
  nothing above requests one while the share has no viewer: no DownTrack exists yet to wait for it. A pub PC whose
  first keyframe was lost, or a track that was already running when a later offer bound it to the share, would then
  leave the share `pending` until the hub's 30 s timeout ends it. So while a share is `pending`
  (`Share.awaitsKeyframe`), every video packet of its layers that is cached and is not a keyframe start asks for
  one, throttled like every other request to one PLI per 500 ms and layer. The first keyframe clears the flag (and
  so does the share's end); a share that has been live never asks this way again.
- **The callers as built**: `Bind` on a sub PC whose gate is open (§9.3); the sub PC reaching `connected` (every
  video DownTrack that isn't paused); the pub PC reaching `connected` again (every video layer attached to its
  tracks; on the first connect no track has arrived yet); a new target of a DownTrack that waits for the target's
  keyframe, when its binding can send; the viewer's PLI or FIR, for the layer forwarded now (else the target);
  every target-layer packet that arrives while the DownTrack waits for a keyframe; and the pending share above.
  A DownTrack whose stream Pion has taken twice in a row without sending any of it asks through the waiting packets
  only once per 5 s (§9.3), so a viewer nothing reaches can't cost the others two keyframes a second.
- **No lock, any goroutine.** The throttle is a compare-and-swap on the layer's `lastPLI`, so DownTrack writers and
  RTCP readers, the layer's own read loop and any Conn's actor call it directly; a caller that loses the race is
  counted as throttled. When the write fails (the pub PC is closing, or went down before its actor saw it),
  nothing is retried: whoever still waits asks again after 500 ms. Audio layers never ask.
- **Nothing for a paused layer** (README S88, §11). A request for a layer that the sharer was told to pause
  (`Share.paused`) returns at once, like one while the pub PC is away (below): no PLI, and `lastPLI` stays as it
  was. The viewer who asks has asked for `high`, the publishing Conn's actor resumes the layer for that, and the
  keyframe request that follows the resume hint must not find the throttle spent on a PLI the sharer got while the
  layer was off.
- **Nothing while the pub PC is away** (README F10, S74-R1). While the publishing Conn's actor has its pub PC as not
  connected (`Conn.pubUp`, the view that the stalled check of §5.3 and the pending share's requests above go by), a
  request returns at once: no PLI, and `lastPLI` stays as it was, so it neither starts the 500 ms nor counts as
  throttled. No PLI could get through then, and the pub PC's connect asks for every layer it carries (the callers
  above; the actor sets `pubUp` before it asks). Before, such a request started the 500 ms, and the connect's own
  request, when it came within them, was throttled. That is the order after an outage that took both a viewer's and
  its sharer's UDP (01 §11.5 B; itest `TestUDPBlackHoleViewer`): the viewer's sub PC connects some 30 ms before the
  sharer's pub PC and asks for the full layer it watches, the share goes live again on the preview layer's keyframe,
  and the viewer's DownTrack, which waits for no keyframe, goes on with the full layer's delta frames. A browser asks
  for a keyframe itself then; the tests' Go viewer doesn't.

### 9.8 Pacing (keyframe bursts)

- One token bucket per video DownTrack: rate = `max(2.5 × the current layer's MaxBitrate after the admin cap (§8.6),
  3 × EWMA bitrate of the current layer, 2 Mbps)`, depth = `max(20 ms × rate, 16 KiB)`. For an 8 Mbps `f` the rate is
  20 Mbps: a 150 KB 1080p keyframe spreads over about 60 ms and a 300 KB frame over about 120 ms, instead of hitting
  the socket in one burst for every viewer. `q` (0.3 Mbps) gets the 2 Mbps floor.
- Why the configured maximum and not only the average: screen video is idle, then bursty. A static screen runs at
  about 1 fps, well under 0.5 Mbps, and the first change arrives at up to the cap, often starting with a 150–400 KB
  frame. A rate from the 2 s EWMA alone would be 2 Mbps at that moment, adding about 1 s of video latency (and A/V
  drift, since audio isn't paced) at each motion onset until the EWMA catches up.
- Audio is never paced. It has its own goroutine, so video pacing never delays it.
- Queue drops feed the allocator (§10.2); at these rates they mean the writer is starved of CPU, not that the pacer
  is slow. Time spent in the pacer is not an allocator input.

**As README S84 built it** (`pacer.go`, `DownTrack.pace`; the tests are §17's `pacer_test`):
- **The bucket starts full** with the DownTrack's first video packet, fills at the rate up to its depth, and holds
  bits; a packet takes its payload's bits and leaves when the bucket is back at zero. The rate is worked out for
  every packet from the layer the packet belongs to (`pacerRate`: the layer's `MaxBitrate` after the admin cap from
  `Share.maxBitrate`, and its measured rate, the ticker's 2 s average; every number an atomic or a constant, so no
  lock), and a lower rate than before (a smaller layer) also lowers what the bucket holds. Time a writer oversleeps
  counts at the next fill, so the rate holds over a burst however coarse the sleeps are; one fill counts at most
  1 s. The wait is the writer's own reused timer, so pacing allocates nothing per packet, and the DownTrack's stop
  or its Conn's close ends it (the packet is then not sent).
- **The keyframe numbers above are from an empty bucket**, as right after another burst. From a full bucket its
  20 ms of the rate leave at once: for an 8 Mbit/s `f` (20 Mbit/s, a 50 KB bucket) a 150 KB keyframe takes about
  40 ms, a 300 KB frame about 100 ms.
- **What goes through it** (decided in README S84, S63's open question): every video packet the writer sends,
  forwarded media and retransmissions alike, in the writer's order, which serves the RTX queue first (§9.5). Audio
  and its retransmissions never are.
- **abs-send-time is stamped after the pacer**: the viewer's receive-side estimator reads the spacing of the
  packets from it (§10.2), so it must be the time a packet leaves, not the time it was taken off the queue.

## 10. Layer selection and downlink adaptation (M1)

### 10.1 Requested quality → layer

| Requested | Layer choice | If none of those layers exists |
|---|---|---|
| `high` | `f`, else `h`, else `q` (a layer exists once its track arrived, even while paused, §11) | `off`, reason `no_layer` (share not live yet) |
| `low` | `q`, else `h` | `off`, reason `no_preview_layer`. A full layer is never sent as a thumbnail: a 25–40 Mbps 4K layer would multiply egress (the plan's "4K60 alone" case) |
| `off` | none | – |
| audio `true` | the audio Layer | `Audio: false` in the state event |

- Effective video quality = `min(requested, cap)`. The cap comes from the allocator (`bandwidth`) or from server
  limits (`server_limit`); the viewer's decoders have a cap of their own (`decoder_unavailable`, `decoder_failed`),
  and `codec_mismatch` is a reason without a cap (README S69, below).
- Audio follows focus: the audio DownTrack is active only while its subscription has `Audio: true`. The client sends
  that for the focused share (05). The SFU doesn't force a single audio stream; a speaker button on another tile just
  sends another update.
- A change of target sets the interest mask to target|current, requests a keyframe, and keeps forwarding the old layer
  until the new one's keyframe arrives (S4: 42–106 ms). Pause is immediate.
- `SubscriptionStateEvent` is sent only when `Forwarded`, `Audio` or `Reason` changes, not on each request.

**As README S52 built it** (`subscription.go`, `downtrack.go`, `share.go`).

- **A layer "exists" from the moment its track is attached until that track ends**, whether or not packets flow on
  it. The table is one function, `selectSlot(kind, want, present)`, over the slot bits of the attached layers.
- **The choice is made again whenever a layer comes or goes**, not only on a request, with the locks of §5.4:
  - *A layer arrives.* Every DownTrack that would rather have it gets it as its target before the track's first
    packet is read, so a viewer who subscribed early gets each layer from its first keyframe and the publisher is
    never asked for one. A viewer who asked for `high` may get the preview layer for a moment, while it is the
    only one there; a thumbnail viewer never gets the full layer (`TestSimulcastFromTheFirstPacket`).
  - *The forwarded layer ends while the share has another one.* The viewer's stream is over at once: a stream does
    not go on from a track that has ended. Nothing is forwarded, the viewer leaves `ShareInfo.Viewers`, and the
    client hears `off` with its reason, until the keyframe of the layer the DownTrack falls back to. The publisher
    is asked for that keyframe right away, once however many viewers wait for it and not at all for a viewer whose
    sub PC can't send, instead of with that layer's next packet, which on a still screen can take a second
    (`TestForwardedLayerEnds`). Viewers of another layer notice nothing.
  - *A track that Pion delivers again with another SSRC* attaches its Layer before the old one is taken off, so
    its viewers wait for the new track's keyframe and don't fall back.
- **The cap is the allocator's.** `Conn.capSubscription(sub, q, reason)` stores the cap and its reason on the
  subscription, applies `min(requested, cap)` and reports at once. Its caller is the downlink allocator (slice 11,
  README S84: `Conn.capWithin` and `Conn.liftCaps`, §10.2), which caps at `low` with `bandwidth`; S52 built the
  cap before anything set it. `server_limit` is later.
- **The decoders' cap is `Subscription.codec`, not `capSubscription`** (README S69, §8.5). It is
  `decoder_unavailable` or `decoder_failed` while the viewer can't decode H.264, and then the subscription gets no
  video, whatever the other cap says; it is empty otherwise. The Conn's actor sets it for every subscription
  whenever the decoder state or the bindings may have changed (`Conn.refreshCodec`: after `SetDecodeCaps` and after
  each applied answer) and reports at once. Kept apart from `capQ` and `capReason`, it can't conflict with the
  allocator's cap; when both hold, the decoders' reason is the one reported.
- **`codec_mismatch` is a derived reason, not a cap** (§8.3). A stream in a profile the viewer's sub PC negotiated
  no payload type for (`DownTrack.mismatched`) leaves the DownTrack waiting, so it starts on the first keyframe it
  can take, and the reason says `codec_mismatch` while the video forwarded is less than the request and no cap
  explains it. The reasons go in this order: the decoders' cap, the allocator's cap, `codec_mismatch`, then what
  the share's layers lack.
- **Reasons**: the two of the table's last column, with one reading the table doesn't spell out. `no_layer` is
  the reason whenever a request for `high` is not served in full: with nothing forwarded, and also while the
  preview layer is forwarded in its place (`Forwarded: low`). A viewer who only waits (for its sub PC, or for the
  keyframe of a layer that serves the request) has no reason (§6.2).
- **Audio** needs no reason: `Audio` in the event is true while the client asked for it and the share's audio
  layer flows to this viewer. Toggling it never renegotiates, and the viewer's sequence numbers are continuous
  across the pause (integration 3).
- **Events** follow §6.2: what the client asked for is answered before `UpdateSubscriptions` returns, what the
  media path changes is reported at most once per 250 ms and subscription.

### 10.2 Downlink allocator (one per subscriber Conn, Galene-style)

**REMB is only an overuse signal, never permission to upgrade.** Chrome's receive-side estimator (libwebrtc's AIMD,
used when the sub PC has abs-send-time and no transport-cc, §8.1) never raises its estimate above about 1.5 × the rate
it currently receives + 10 kbps, and raises it by only about 8 %/s. It lowers the estimate only on overuse, to about
0.85 × the received rate when that is lower, and then sends a new REMB at once (a fall of 3 % or more). So a REMB below a layer the
viewer isn't receiving yet says nothing about the link, and a viewer held on `low` could never report a REMB high
enough to earn `high` back. Screen video makes this worse: a static screen at about 1 fps keeps the estimate low,
then motion raises the forwarded rate to the cap within a second while REMB grows at 8 %/s.

Inputs: the viewer's REMBs, the loss EWMA from RRs of its video DownTracks, queue drops, the rate currently forwarded
to that viewer (video and audio, 2 s EWMA of its DownTracks' egress counters), and the measured bitrate of each layer
(2 s EWMA, §9.1).

- **Overuse REMB**: a REMB at least 3 % below the previous one from this viewer and below the rate currently forwarded
  to it. The previous REMB counts only if it arrived less than 5 s earlier. After a longer gap (for example while no
  video was forwarded) the new REMB is only a new baseline.
- **Congestion**: an overuse REMB, **or** loss ≥ 10 % averaged over 2 s, **or** more than 50 queue drops in 1 s.
- **Budget**, set only on congestion: on an overuse REMB, `0.9 × that REMB`; on loss or drops, `min(0.9 × latest REMB,
  0.85 × forwarded rate)`, or `0.85 × forwarded rate` when no REMB is known.
- **Capping** fills the budget cheapest first:
  1. audio;
  2. every `low` (thumbnail): always kept, never turned off by the server. The client decides how many thumbnails a
     phone shows;
  3. `high` requests, the most recently requested first. Each is served at `f` if the layer's measured rate fits in
     what is left, else capped at `q` with reason `bandwidth`. So the oldest `high` requests are capped first.
- A REMB is never compared with a target the viewer isn't receiving yet. In `normal`, a new or raised target (focus
  change, share start, a layer resuming after pausing, §11) is forwarded at once and is never capped by the budget.

| State | Condition | Action |
|---|---|---|
| `normal` | congestion | set the budget, cap the `high` requests that don't fit → `limited` (backoff = 10 s) |
| `limited` | congestion | lower the budget, cap any `high` requests that no longer fit; the backoff period starts again |
| `limited` | a new `high` request | served at `f` only if the layer's measured rate fits the budget, else capped |
| `limited` | backoff elapsed and loss < 2 % over the last 3 s | remove the caps (trial upgrade) → `probing` |
| `probing` | within 6 s: a REMB at least 3 % below the previous one, **or** loss ≥ 5 %, **or** more than 50 queue drops in 1 s | set the budget from it (as above), cap again, backoff = min(2 × backoff, 120 s) → `limited` |
| `probing` | 6 s pass cleanly | backoff = 10 s → `normal` |

Why trial upgrades: since REMB can't grant an upgrade, the allocator tries one. On a link that still can't carry `f`,
the trial overuses it, REMB falls within a second or two, and the viewer goes back to `low`: a short quality dip at
most every 2 minutes. Once the link has recovered, the next trial passes, so a throttled viewer returns to `high`
within one backoff period (at most 2 minutes) without a reload. Right after a trial starts, Chrome's estimate is
still near the old, lower rate, and an overuse can't raise it to 0.85 × the new rate. So a trial on a link that still
can't carry `f` usually fails on loss (the bottleneck drops the excess), not on a falling REMB. A jump from an idle
screen to motion is never compared with REMB, so it can't cause a downgrade; only a falling REMB (real overuse), loss
or drops can. TWCC downlink in v2 replaces trial upgrades; the allocator's interface stays.

**As README S84 built it** (`allocator.go`: `allocator.step`, `allocate`, and on the Conn `adapt`, `capWithin`,
`liftCaps`, `requestChanged`; the tests are §17's `allocator_test` and integration 16), where the text above leaves
a choice:
- **Who does what.** The RTCP readers of the Conn's DownTracks put what the viewer's REMBs and receiver reports say
  under the allocator's own lock, a leaf (§9.3, §5.4); both queues count their drops in an atomic. The decisions
  are the Conn's actor's (`Conn.adapt`): once a second, when the ticker wakes it with this second's rates in place,
  and at once when a REMB falls or a report shows 5 % loss or more. A backoff's end and a trial's end are seen by
  those looks, so each takes effect up to 1 s after its time: a trial starts up to a second after its backoff, and
  passes up to a second after its 6 s. A change of state is logged at debug, with the cause and the budget.
- **The loss input is what the link lost**, with four rules the text above doesn't spell out:
  - the own sequence numbers the SFU never sent (an upstream gap that no repair filled, packets a queue dropped;
    `seqGaps`, §9.3) are taken out of both the packets expected and the packets lost: the viewer counts them as
    lost, but the link didn't lose them;
  - a report that comes more than 2 s after the last one of its stream covers an outage of the viewer's network,
    not congestion (its RTCP didn't reach the SFU either, which downlink congestion doesn't do: the reports travel
    the other way), and only starts the count again;
  - a report is evidence about the allocator's last decision (a cap, or the start of a trial) only when all of the
    time it covers came after that decision: a report covers the second or so before it, so one that began before
    a trial says what the viewer got before the trial;
  - a loss verdict needs at least 20 packets: one lost of three is no 33 % of anything.

  "Loss averaged over 2 s" is then the packets lost over the packets expected, summed over the reports of the last 2 s
  that pass these rules, with no EWMA; "loss < 2 % over the last 3 s" is the same over 3 s, and fewer than 20 packets is
  no loss. The reports are kept for 3 s, at most 1024 per Conn, and one that comes less than 200 ms after the last that
  counted for its stream is folded into the next (§12). S84 found the rules needed: without them the black-hole cases of
  01 §19 in `internal/server/itest` failed. Pion's receivers, like Chrome's, also count a packet that came back on the
  RTX stream as lost, so repair shows up as loss: 10 % of the video lost and repaired is congestion by this section,
  which is why integration 4 loses 5 % (§17).
- **The budget.** On an overuse REMB, 0.9 × the lowest overuse REMB since the actor's last look; on loss or drops,
  min(0.85 × the forwarded rate, 0.9 × the latest REMB when one came in the last 5 s). During a trial any REMB that
  fell by 3 % fails it, overuse or not, and the budget is 0.9 × that REMB. In `limited`, congestion only ever lowers
  the budget (the lower of the old and the new one) and starts the backoff again. Drops are a rate measured over at
  least 0.9 s.
- **The allocation** (`allocate`) weighs every subscription at measured rates: the share's audio when the client asks
  for it; a `low` at its preview layer's rate; a `high` at its full layer's rate, or at its preview layer's once capped.
  **A layer paused for its sharer, or resumed less than 2 s ago, weighs its maximum instead** (`Share.allocRate`, README
  S88, review S88-R1): a paused layer measures 0 within seconds, and a resumed one less than it sends while its 2 s
  moving average catches up, so a `high` for a paused share in `limited` would always "fit" and resume at up to 8 Mbit/s
  for a viewer the allocator knows is congested. It is capped unless the full layer's maximum fits; the request still
  counts for the uplink policy, so the layer resumes, and the next trial tests a layer that flows (§11). The `high`
  requests are served newest first, by when the client last asked for `high` after asking for less
  (`Subscription.highAt`). A subscription whose viewer can't decode the video (§8.5) weighs its audio only.
- **A share without a smaller layer is never capped.** A `high` request whose share has no other layer for a cap at
  `low` to give (a single-layer share, or one with only its preview layer) keeps the layer it has, whatever the
  budget: a cap would leave its viewer with no video at all. Its rate still counts against the budget.
- **While `limited`, any change to a subscription's request decides every cap again** (`requestChanged` →
  `capWithin`), newest `high` first, before the client hears what its request got. A new `high` is served at `f`
  only if its rate fits, as the table says, and a cap that no longer needs to be is lifted too: when a request
  goes down to `low` or `off`, an older `high` may get its full layer back. In `normal` and `probing` nothing is
  capped, and a new or raised target is forwarded at once.
- **The caps** are the subscriptions' server cap (§10.1), at `low` with the reason `bandwidth`; a trial lifts every
  cap at once (`liftCaps`), and a trial that fails caps again within its new budget. Every cap the client hears is a
  `bandwidth` downgrade in `Metrics` (§13).
- **`ConnStats.DownlinkEstimate`** (§13) is the budget while `limited`, else the viewer's latest REMB while it is
  less than 5 s old, else 0.
- **The trial in integration 16 fails on a falling REMB, not on loss.** The text above expects a trial on a link
  that still can't carry `f` to fail on loss more often. With `rembsim` as §17 specifies it, the estimate never
  falls without overuse, so it is still at the throttled link's rate when the trial starts; the trial's overuse
  then cuts it to 0.85 × the ramping received rate within about 250 ms, before a receiver report shows the loss.
  It did so in every run, so the test wants the trial to fail for either reason and logs which. Either cause fails
  a trial, so a real Chrome may come first with either.

*Later (M5)*: `server_limit`, a server-wide egress soft cap: while total egress (2 s EWMA) is above it, new or
upgraded `high` requests are capped at low. M1 has no such setting (03 stores none); the reason value is reserved.

## 11. Publisher-side control (uplink)

```go
type UplinkPolicy interface { // one per Share, asked on the publishing Conn's actor (below)
	Tick(s ShareUplinkStats) UplinkDecision
}
type ShareUplinkStats struct {
	Client       ClientKind
	HighRequests int                // the share's viewers whose request is high, before the allocator's cap
	Layers       []LayerUplinkStats // the video layers attached now, sorted f, h, q; paused ones included
}
type LayerUplinkStats struct {
	RID     string
	Bitrate int           // bps, 2 s moving average (LayerInfo.Bitrate)
	LossPct float64       // loss over the last 2 s, in percent (LayerInfo.LossPct)
	Jitter  time.Duration // interarrival jitter (RFC 3550 §6.4.1, §9.1)
	Viewers int           // video DownTracks that forward the layer now or wait for its keyframe
}
type UplinkDecision struct {
	MaxBitrate int             // bps; 0 = none
	Active     map[string]bool // per rid; nil = no change
}
const HintReasonAdmin, HintReasonViewers = "admin", "viewers" // QualityHintEvent.Reason (README S88)
```

- **M1, every publisher**: `MaxBitrate` = `Limits.MaxShareKbps` (03 setting `maxShareBitrateKbps`, pinnable by 04's
  `limits.max_bitrate_kbps`; 0 = none). When it is set, the SFU sends REMB
  `{bitrate, SSRCs of the video layers}` on the pub PC every 1 s. When the value changes, it sends
  `QualityHintEvent{Reason: "admin"}` whose `Encodings` is the full recomputed list, with `f.MaxBitrate` capped per
  §8.6. Chrome applies REMB as an upper bound under its own GCC. The web sharer also applies the encodings through
  `setParameters` (05).
- **M1 stretch, layer pausing** (04 config key `sfu.pause_unwatched_layers`, default on; cuttable to M5 without a
  protocol change): when no video DownTrack has requested `high` for 10 s, send `QualityHintEvent{Reason: "viewers"}`
  with the full `Encodings` and `f.Active` false. As soon as one does, send it again with `f.Active` true, and a PLI.
  "Requested" is the viewer's request before the allocator's `bandwidth` cap (§10.2), so `f` keeps flowing while a
  viewer is capped, and its trial upgrades test a real layer rather than one measured at 0 bps. As README S84 left it
  for S88: that is `Subscription.reqVideo` (the client's request, which the actor holds), not the effective quality
  under `capQ` and not what a DownTrack forwards; a viewer whose `high` is capped at `low` still counts as requesting
  `f`. Browsers apply the list with `setParameters` (05). This saves the sharer about 8 Mbps of upload while nobody
  focuses the share, at the cost of 0.2–0.5 s more on the first focus. `q` is never paused while anyone is subscribed. A
  paused layer stays in `ShareInfo.Layers`, and pausing never changes `room.state` (§5.3).
- Every hint carries both the current cap and the current pause state. When a single hint carries both a cap change
  and a pausing change, the pausing change wins: `Reason` is "viewers".
- **Later (M2), native publishers**: a loss and receive-rate estimator per share (Galene's estimator design). It sends
  REMB plus `QualityHintEvent{MaxBitrate}` with the plan's hysteresis (changes of at least 15%, at most every 2 s).
  Browsers keep their own GCC over TWCC.

**As README S88 built it** (`uplink.go`, with `Share.paused`, `Share.resumedAt` and `Share.up` in `share.go`,
`Layer.sampleJitter` in `layer.go`, `Conn.resendHints`; the tests are §17's), where the text above leaves a choice
or the code goes past it:
- **Who asks the policy, and when.** Each share has an `UplinkPolicy` (M1's `m1Uplink`, the same for every
  publisher) that the publishing Conn's actor asks: once a second (the ticker wakes every Conn that publishes,
  `Conn.uplinkTick`), at once when `SetLimits` may have changed the cap (§6.1), and at once when a viewer's request
  becomes `high` (`Share.askedForHigh`, posted by the viewer's actor after it published the request). The last one
  is posted whenever pausing is on, not only while a layer is paused: a look that is deciding to pause right now
  stores the paused bit only after it decided, and the look posted here, which sees the new request, comes after
  it, so a resume never waits up to 1 s for the next tick. The ticker's and `SetLimits`' looks, which take every
  share the Conn publishes, end with the pub PC's REMB (below); a viewer's look takes only its share and writes none.
- **The cap** is `Limits.MaxShareKbps` for every share, in bit/s (`Limits.shareCap`, at most 2^30 kbit/s).
- **One REMB per pub PC, not per share.** Chrome applies a REMB's bitrate as the upper bound of everything the PC sends,
  whichever SSRCs it names, so while every share on the PC has a cap the PC gets one REMB a second (and one at once
  after `SetLimits`): the **sum of its shares' caps**, naming every video SSRC of those shares (paused layers too, audio
  not), sorted; its sender SSRC is the pub PC's RTCP SSRC (§9.7). **None while any share on the PC has no cap**: a REMB
  can't say "no limit" for one share. **When the cap goes**, a browser keeps the last REMB as its limit until another
  one arrives, so the PC gets **three REMBs of 1 Gbit/s** (`rembUncapped`, one per such look; one may be lost), then
  none. 0 is avoided: libwebrtc reads it as no limit, but a receiver that reads it literally would stop sending. Nothing
  is written while the actor has the pub PC as away, and a new pub PC (a new `gen`) is owed no lifts from the old one's
  REMBs.
- **A hint when the cap or the pause state changes.** A look sends a `QualityHintEvent` when the cap differs from
  what the sharer was told last (by `StartShare`'s `ShareParams` or a hint; `UpdateShare`'s don't count, §6.1) or
  the paused layers changed; the reason is `viewers` when the pause state changed, else `admin`. Every hint carries
  the full encodings of the current preset under the cap with the paused layers inactive, and `MaxBitrate`. So an
  `admin` hint also goes out when the cap changes without changing `f`'s encodings (a cap of 8 Mbit/s or more), and
  for a share that is still `pending`.
- **Pausing** (with `sfu.pause_unwatched_layers`, on by default, README §6 "Decided after group 13"): the full
  layer is paused once no video DownTrack has requested `high` for 10 s, and **the 10 s are counted from the
  share's start (`StartShare`), or from the first look that found no request for `high` after one that found
  some**: the last request ended between those two looks, a second apart at most, so the layer is paused 10 to
  11 s after it ended, never before (review S88-R2). **Any new request for `high` resumes it**, also one from a
  viewer whose decoders can't take the video (`decoder_unavailable`, `decoder_failed`): it is the request that
  counts, as the text above says for the allocator's cap. **A share without a preview layer is never paused**: its
  full layer is the only video it has (a single-layer share, which the text above doesn't cover). `q` is never
  paused, and `h` (M5) neither.
- **The resume.** The hint with `f` active goes out first, then one keyframe request for the layer (a sharer that
  sends it again starts with a keyframe of its own accord; this asks one that never stopped it, a client that
  applies no encodings). While a layer is paused, keyframe requests for it do nothing and don't start the 500 ms
  throttle (§9.7), so that request goes out. The time of the resume goes to `Share.resumedAt`, before the paused
  bit is cleared, for the downlink allocators: a paused layer, and one resumed less than 2 s ago, weighs its
  maximum (§10.2, `Share.allocRate`).
- **The uplink control is the publishing Conn's actor's**, like everything else the Conn publishes (§5.4): the
  policy, the cap and pause state the sharer was told (`shareUplink`), and the REMB state. Other goroutines read
  two atomics: `Share.paused` (the slot bits of the paused layers) and `Share.resumedAt`. The viewers' requests
  reach the policy as each subscriber's actor published them (`Subscription.requested`, from
  `Subscription.shown`). `uplinkStats` reads the layer set under `Share.mu`, a leaf, and atomics otherwise (review
  S88-R5).
- **`ShareUplinkStats` as built**: `HighRequests` counts the share's viewers, not its layers; `Layers` lists the
  video layers attached now, paused ones included; `LayerUplinkStats.Viewers` counts the video DownTracks that
  forward the layer or wait for its keyframe; loss is `LossPct`, in percent like `LayerInfo`, not a fraction; and
  `Jitter` is §9.1's per-frame estimate. M1's policy reads only `HighRequests` and the layers' rids; the rest is
  for M2's estimator.
- **`UpdateShare` and `Resync` say the same as the hints** (§6.1, §6.2): `UpdateShare`'s encodings carry the cap
  as it is now and the pause state, and `Resync` sends each share's last hint again, rebuilt from the current cap,
  pause state and preset with the last hint's reason.
- **The Go publisher can follow a hint** (§15.1): `publish.Publisher.SetActive` turns a video layer off and on
  again, and `sfutest.ApplyHint` does with a hint's encodings what a browser does with `setParameters`.
- **Logs**: a hint is a debug line (`quality hint`, with the share, reason, cap and paused rids); a REMB that
  can't be written is a debug line.

## 12. Limits and guards (M1)

Guards limit abuse, not people. Every number here is a constant in `config.go`, except the soft limits.

| Guard | Value | On violation |
|---|---|---|
| PCs per Conn | 1 pub + 1 sub | `sfu.pc_limit` for an offer whose `PCKind` is above `PCSub`; a second PC of one kind can't be asked for (§6.1 "PC limits") |
| PC creations | 10 per minute per Conn and PC kind, counting the PCs a client makes the Conn create (the scope is below); internal rebuilds (§8.5) are not counted and never refused | `sfu.pc_rate_limited` (retryable); `Error.RetryAfter` = time until the oldest counted creation leaves the 1-minute window; the call's error, or an `ErrorEvent` about the sub PC (§6.1 "PC limits") |
| ICE + DTLS handshake | 10 s to `connected` from the PC's first answer (§5.3), new PCs only (not ICE restarts); DTLS context 10 s | close PC, `PCStateEvent{failed, handshake_timeout}` |
| PC after `failed` | 30 s grace | close, `PCStateEvent{closed, pc_grace_expired}`; pub shares stay `stalled` (the hub's 30 s timeout ends them) |
| Shares | 4 per participant (defense in depth; the hub enforces 4 per user and the room soft limit, 01) | `sfu.too_many_shares` |
| Publish offer | ≤64 KiB, ≤8 m-lines, ≤2 rids from {f,q}, no data channel | §6.3 codes |
| Remote candidates | 64 per PC and gen in total, trickled or in the PC's remote descriptions, buffered or applied; a candidate is what Pion keeps as one, and one that comes again counts once (§7.3); the first 64 are kept | later ones dropped, logged at debug |
| Peer-reflexive sources (04's netx, 04 §7.8) | 64 distinct remote transport addresses per local ICE ufrag, from which STUN with that ufrag reaches Pion: UDP and ICE-TCP, every UDP socket and both TCP ports counted together, probe PCs included; an address counted once costs nothing again; the count starts over with a new ufrag (ICE restart) and ends when Pion removes the ufrag from the muxes (close, failed); a removed ufrag is never counted again (Pion never reuses one); sources count before Pion checks MESSAGE-INTEGRITY, so an on-path party who spoofs UDP sources with a seen ufrag can use up its slots until an ICE restart | STUN from a further address is dropped before Pion: no Binding response, no prflx candidate, no address-map entry; an ICE-TCP connection is closed at its first frame; logged at debug once per ufrag |
| Subscriptions | 256 per Conn; 64 items per `UpdateSubscriptions` | `sfu.too_many_subscriptions` |
| Actor command queue | 64; 5 s wait | `sfu.busy` |
| Share without media | none here: the hub ends shares `starting` or `stalled` for 30 s (`media_timeout`, 01 §4.4) | – |
| Keyframe requests upstream | 1 per 500 ms per layer | throttled (counted) |
| NACK | ≤1 s old, ≤3 retransmits per seq, 25% budget | skipped (counted) |
| DownTrack queues | video 1024, audio 256, RTX 256 | drop newest (counted) |
| The viewer's RTCP for the downlink allocator (README S84, §10.2) | a receiver report of a video stream counts at most once per 200 ms; at most 1024 reports kept per Conn, each for 3 s; a REMB counts only above 0 and up to 100 Gbit/s | a report sooner than 200 ms is folded into the next one; past 1024 the oldest goes; any other REMB is ignored. A viewer that floods reports costs no more than one that reports five times a second (`TestAllocatorReportFlood`) |
| Probe PCs | 1 per user and transport (a new one closes the old one), 20 total, 20 s lifetime | `sfu.probe_limit` (retryable; `RetryAfter` at least 1 s) |
| Probe offer (README S76) | ≤64 KiB, exactly one m-section, `m=application`; its remote candidates through the filter of §7.3 and capped at 64, as a PC's | `sfu.bad_sdp`; candidates past the cap dropped, counted at debug |
| Probe data channels (README S76) | 1 per probe PC; no echo while more than 1 MiB of echoes waits to go out | a further channel is closed as it opens; a message that comes while 1 MiB waits is not echoed |
| UDP buffers | 8 MiB requested by 04's netx | warning in doctor (04) |

ICE-TCP first STUN 10 s and ICE-TCP per-IP limits are 04's (netx TCPMux on 443 and 7882); a violation closes the TCP
connection, not the PC.

**From the M1 security review** (a read-only review of `main` at f7053dc; README §6 "Decided after group 10" (1)).
Two of its findings are about state that one PC grows from crafted packets, where "Pion has no cap of its own":
- **R3, peer-reflexive sources**: a member who has a PC's ICE credentials could send valid checks from ever new
  source addresses, and Pion makes a prflx candidate and pairs for each one, past the 64 of the remote-candidate
  row. Fixed below Pion, in 04's netx (README F06): the peer-reflexive sources row of this table, 04 §7.8.
- **R1, pub-PC tracks under SSRC churn**: a publishing member with a custom DTLS-SRTP client can make Pion deliver
  `OnTrack` again for each new SSRC, and the pub PC then grows tracks, reader goroutines and NACK-filter entries without
  a bound. README F07 caps the deliveries a pub PC holds and drops the PC past the cap; its change is built but not
  merged, **waiting for the owner** (its tests are not written yet; still so after group 12). Until it lands, nothing in
  this table bounds them. README F06 found a related cost of SSRC churn below `OnTrack`: for every authenticated SSRC
  that no m-section declares, Pion keeps an SRTP and an SRTCP read stream until DTLS closes, whose buffers nobody reads
  (pion/srtp lets them grow to 1 MB and 100 KB). netx can't bound that below the mux, where it sees source addresses and
  no PCs; a per-PC `SettingEngine.BufferFactory`, which Pion calls with the SSRC of every new read stream, could, in the
  SFU. That is an option for F07, not built.

**What the PC creation limit counts** (README S57, `Conn.admitPC`; one window of the last 10 per Conn and kind):
- **pub**: every pub PC the SFU makes for an offer of a new `gen`, whether or not Pion then takes that offer. An
  offer that the SFU's own checks refuse (§8.4 step 1) makes no PC and counts for nothing, and neither does a
  re-offer in the current `gen`.
- **sub**: every sub PC after the Conn's first that a client's request builds: `ResetPC`, and the successor of a
  closed sub PC whichever request builds it (`ResetPC`, `RestartICE`, `Resync()` or a subscription change,
  §5.3). Such a PC follows one that the client closed or made the SFU close, at the client's next request, so
  it is the client's doing on every one of the four paths.
- **Not counted**: the Conn's first sub PC, which a Conn gets whatever its client does and can get only once, so
  ten `ResetPC`s within a minute pass; a `ResetPC` on a Conn without subscriptions, which closes the sub PC and
  builds none; and the SFU's own codec rebuilds (§8.5, README S69).
- A refused creation is not counted either, and the PC the Conn has is left as it is. The hub's own limits come
  first (01 §13: 6 pub offers with a new `gen` and 12 `pc.restart` a minute per PC).

Admin soft limits (0 = unlimited, the default; editable in the admin UI and stored by 03; pushed live with
`SetLimits` by the wiring from `SettingsCache.OnChange`, 04 §6.6). The room share limit and the participant limit
are enforced by the hub (01), not here.

```go
type Limits struct {
	MaxShareKbps int // cap per share: REMB, hint (§11) and ShareParams (§8.6); 03 maxShareBitrateKbps
}
// later: type RoomLimits struct{ MaxShares, MaxShareKbps int } and a server-wide egress cap (§10.2)
```

Config keys: the ICE ports, IP families, loopback candidates and socket buffers are 04's keys (`listen.ice_udp`,
`listen.ice_tcp`, `listen.https` (ICE-TCP on 443), `network.ipv6`, `network.include_loopback`,
`network.udp_buffer_bytes`, `network.exclude_interfaces`; 04 §4.3) and reach the SFU only through
`Config.Transport`. The only SFU key, registered in 04's `config/keys.go`:

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

- `(*SFU).Snapshot() Snapshot`: rooms, then per share its `ShareInfo` (whose `Layers` carry per-layer ingress:
  size, fps, bitrate, loss), ingress and egress bps and viewer counts by forwarded quality; per room, one
  `ConnSummary` per Conn; plus totals. This feeds the admin dashboard (04, 05): "bitrate, layer, loss and egress".
  04's adapters map the fields one to one.
- Per room, one `ConnSummary` per Conn: the sub PC's selected transport (`udp` | `tcp443` | `tcp7882`, labeled as in
  §7.1) and RTT from candidate-pair stats, falling back to the pub PC; "" while not connected. `ConnID` is the id 01
  passes in `JoinParams.Conn`. 04 fills the dashboard's `connections[].transport` and `rttMs` from it. As README
  S79 built it, the transport and the RTT fall back to the pub PC's each on its own: the sub PC's transport is ""
  while it isn't connected, and its RTT 0 until its first sample.
- Probe PCs (§7.6) are not counted in `Snapshot` or `Metrics`: no PC count and no PC metric includes them.

```go
type Snapshot struct {
	At     time.Time
	Rooms  []RoomSnapshot
	Totals SnapshotTotals
}
type RoomSnapshot struct {
	Room   RoomID
	Shares []ShareSnapshot // by StartedAt
	Conns  []ConnSummary   // by ConnID
}
type ShareSnapshot struct {
	Info                  ShareInfo
	IngressBps, EgressBps int64 // RTP level, 2 s EWMA
	Viewers               ViewerCounts
}
type ViewerCounts struct {
	High, Low, Audio int // DownTracks forwarding now, by forwarded quality; Audio = audio DownTracks forwarding
}
type ConnSummary struct {
	Conn      ConnID
	User      UserID
	Transport string        // "udp" | "tcp443" | "tcp7882" | "" (§7.1)
	RTT       time.Duration // selected candidate pair
}
type SnapshotTotals struct {
	IngressBps, EgressBps int64          // RTP level; EgressBps is the dashboard's media.egressBps (§15.2)
	DownTracks            int
	PCsByState            map[string]int // pub+sub PCs by webrtc.PeerConnectionState string
	PCsByTransport        map[string]int // connected pub+sub PCs by §7.1 label
}
```

- `(*Conn).Stats() ConnStats`: per PC (state, selected transport, RTT), per own share (layers, bitrates, loss), per
  subscription (requested, forwarded, reason, layer, bitrate, NACKs served, drops, profile), and the downlink
  estimate. Signal sends it as `stats` (01) every 2 s while the client asks for it. S29 fixed its shape
  (`stats.go`); sfuplane converts it to `protocol.ServerStats` (01 §8.11, §15.4):

```go
type ConnStats struct {
	PCs              []PCStats           // pub first, then sub; only the PCs that exist
	Shares           []OwnShareStats     // the shares this Conn publishes, by StartedAt
	Subscriptions    []SubscriptionStats // by share id
	DownlinkEstimate int64               // bit/s for the sub PC, from the downlink allocator (§10.2); 0 while unknown
}
type PCStats struct {
	Kind      PCKind
	Gen       uint32
	State     string        // a webrtc.PeerConnectionState string
	Transport string        // "udp" | "tcp443" | "tcp7882" | "" while not connected (§7.1)
	RTT       time.Duration // of the selected candidate pair
}
type OwnShareStats struct { // the ingress of one share the Conn publishes
	Share        ShareID
	State        ShareState
	Layers       []LayerInfo // the video layers, as in ShareInfo
	Audio        bool        // an audio track is attached
	AudioBitrate int         // bps, 2 s EWMA
	AudioLossPct float64
}
type SubscriptionStats struct {
	Share          ShareID
	Requested      Quality
	Forwarded      Quality
	Reason         SubReason
	Layer          string     // rid of the video layer forwarded now; "" when none
	Profile        ProfileKey // H.264 profile of the forwarded video; "" when none
	AudioRequested bool
	AudioForwarded bool
	Video          DownTrackStats
	Audio          DownTrackStats
}
type DownTrackStats struct { // the egress numbers of one DownTrack
	Bitrate     int     // bps, 2 s EWMA, media and retransmissions
	LossPct     float64 // from the viewer's last receiver report about the track
	NACKsServed uint64
	Drops       uint64 // packets dropped by the DownTrack's queues
}
```

  The types don't change when the numbers arrive. Until then `Stats`, `Snapshot` and `Metrics` hold what the
  object model knows: after S29, `Stats` lists the Conn's own shares with their attached layers (`RID` only), and
  `PCs`, `Subscriptions` and `DownlinkEstimate` are empty; `Snapshot` lists rooms, shares and Conns without rates,
  viewer counts, transports or totals; `Metrics` has every label value of the table below present, with only
  `Conns` and `Shares` counting. The media path (slice 6, README S41) and slice 12 (README S79) fill in the rest.
  Since README S79 everything is filled in but `ConnStats.DownlinkEstimate` and the `bandwidth` downgrades, which
  came with the downlink allocator (slice 11, README S84: the estimate is the allocator's budget while it caps,
  else the viewer's latest REMB while it is fresh, §10.2), and `server_limit`, which has no source in M1.
- `(*SFU).Metrics() Metrics`: plain atomics, cheap to read. 04 maps them to Prometheus (optional, off by default):

| Metric | Type | Labels | Label values |
|---|---|---|---|
| `isshoni_sfu_conns` | gauge | `role` | the `Role` values: `full`, `viewer`, `publisher`, `agent` |
| `isshoni_sfu_peerconnections` | gauge | `kind`, `state` | kind: `pub`, `sub` (probe PCs are not counted); state: the `webrtc.PeerConnectionState` strings `new`, `connecting`, `connected`, `disconnected`, `failed`, `closed` |
| `isshoni_sfu_selected_transport` | gauge | `transport` | `udp`, `tcp443`, `tcp7882` (connected pub+sub PCs, labeled by the §7.1 rule; a pair labeled "" is not counted) |
| `isshoni_sfu_shares` | gauge | `state` | the `ShareState` values: `pending`, `live`, `stalled` |
| `isshoni_sfu_downtracks` | gauge | `kind`, `forwarding` | kind: `video`, `audio`; forwarding: `high`, `low`, `off` (a forwarding audio DownTrack counts as `high`) |
| `isshoni_sfu_ingress_bytes_total`, `isshoni_sfu_ingress_packets_total` | counter | `kind` | `video`, `audio` (publisher RTX is unwrapped into `video` by Pion, §9.1) |
| `isshoni_sfu_ingress_duplicates_total` | counter | | incoming packets already in the cache (Chrome's redundant RTX copies, §9.1), dropped without fan-out |
| `isshoni_sfu_egress_bytes_total`, `isshoni_sfu_egress_packets_total` | counter | `kind` | `video`, `audio`, `rtx` (every retransmission from the cache, RTX-wrapped or plain) |
| `isshoni_sfu_nack_received_total`, `_nack_served_total`, `_nack_missed_total`, `_rtx_skipped_total` | counter | | |
| `isshoni_sfu_pli_sent_total`, `_pli_throttled_total`, `_keyframes_received_total` | counter | | |
| `isshoni_sfu_layer_switches_total` | counter | | |
| `isshoni_sfu_downgrades_total` | counter | `reason` | the non-empty `SubReason` values: `bandwidth`, `server_limit` (later; 0 in M1), `codec_mismatch`, `decoder_unavailable`, `decoder_failed`, `no_preview_layer`, `no_layer` |
| `isshoni_sfu_queue_drops_total` | counter | `kind` | the DownTrack queues: `video`, `audio`, `rtx` |
| `isshoni_sfu_handshake_timeouts_total` | counter | `kind` | `pub`, `sub` |
| `isshoni_sfu_packet_cache_bytes` | gauge | | |

```go
// Metrics is a copy of the SFU's counters and gauges. Map keys are the label values in the table above; every
// listed value is present, zero or not. Probe PCs are never counted.
type Metrics struct {
	Conns             map[string]int64            // isshoni_sfu_conns{role}
	PeerConnections   map[string]map[string]int64 // isshoni_sfu_peerconnections{kind}{state}
	SelectedTransport map[string]int64            // isshoni_sfu_selected_transport{transport}
	Shares            map[string]int64            // isshoni_sfu_shares{state}
	DownTracks        map[string]map[string]int64 // isshoni_sfu_downtracks{kind}{forwarding}
	IngressBytes      map[string]uint64           // isshoni_sfu_ingress_bytes_total{kind}
	IngressPackets    map[string]uint64           // isshoni_sfu_ingress_packets_total{kind}
	IngressDuplicates uint64                      // isshoni_sfu_ingress_duplicates_total
	EgressBytes       map[string]uint64           // isshoni_sfu_egress_bytes_total{kind}
	EgressPackets     map[string]uint64           // isshoni_sfu_egress_packets_total{kind}
	NACKReceived      uint64
	NACKServed        uint64
	NACKMissed        uint64
	RTXSkipped        uint64
	PLISent           uint64
	PLIThrottled      uint64
	KeyframesReceived uint64
	LayerSwitches     uint64
	Downgrades        map[string]uint64 // isshoni_sfu_downgrades_total{reason}
	QueueDrops        map[string]uint64 // isshoni_sfu_queue_drops_total{kind}
	HandshakeTimeouts map[string]uint64 // isshoni_sfu_handshake_timeouts_total{kind}
	PacketCacheBytes  int64
}
```

  No per-share or per-user labels (unbounded cardinality). Per-share data is in `Snapshot`. Month-to-date transfer
  is counted at the socket layer by 04's `netx.TransferCounter` (04 §11.3), not from these counters.
- **Logs** (slog, `component=sfu`): Conn join and leave (IDs only), PC state changes with the selected transport type,
  share lifecycle, codec-policy changes, downgrades (info); handshake timeouts, sub offers re-sent for lack of an
  answer (warn). Never SDP, never ICE candidates or client IPs above debug level, never tokens.

**What README S79 settled** (slice 12; `stats.go`, `counters.go`, `pionlog.go`), where the text above leaves a
choice:
- **None of the three reports asks an actor or waits.** They read what the Layers and DownTracks measure (their
  rates are the ticker's, once a second), what each Conn publishes of its PCs (`Conn.views`: Pion keeps the state
  and the selected pair in atomics) and of its subscriptions (`Subscription.shown`: the request and the reason the
  client last heard), and the SFU's totals.
- **Rates** are 2 s moving averages of RTP payload, up to a second old. A share's `IngressBps` is the sum of its
  attached layers' bitrates; its `EgressBps` and a DownTrack's `Bitrate` include the retransmissions.
- **Transport and RTT only while connected**: `PCStats.Transport` and `RTT` are set only while the PC is
  `connected` (before, there is no pair; after, what was measured no longer holds). The RTT is what the PC's RTCP
  readers last read from the selected pair's statistics, every 5 s: a sub PC's DownTracks (§9.3), and since S79 a
  pub PC's track readers too (`samplePairRTT`), for a `ConnSummary` whose sub PC has none.
- **`Snapshot.Totals` maps hold every label**: `PCsByState` and `PCsByTransport` always have every state and every
  transport label, zero or not, as `Metrics` does. `PCsByState` counts each Conn's current pub and sub PC (a closed
  sub PC that is still the Conn's counts as `closed`), `PCsByTransport` the connected ones.
- **Counters are totals per SFU** (`counters`): every Layer or DownTrack counter that feeds a counter of the table
  is a tally that also counts into the SFU's total of the same thing, at the same moment, so a counter never goes
  back when a share, a layer or a subscription ends. The gauges are read from the object model when `Metrics` is
  called.
- **A layer switch** (`isshoni_sfu_layer_switches_total`) is a viewer's video stream going on in another layer
  than the one it was in, at once or after a pause. A resume in the same layer, or a publisher's new track for the
  same layer, is none; the DownTrack's own `switches` still counts every new epoch.
- **A downgrade** (`isshoni_sfu_downgrades_total{reason}`) is a `SubscriptionStateEvent` whose reason is not empty
  and is not the one its client heard last (`Conn.tell`), `no_layer` for a share that is still pending included; a
  `Resync()` that sends the same state again counts nothing. Each is logged at info ("subscription downgraded",
  with the share id, the requested and forwarded quality and the reason).
- **Log levels**: the PC state line carries `transport` (the §7.1 label, never an address) on the `connected` line
  only. Handshake timeouts are warnings (they were info before S79), and so are sub offers sent again (they were
  debug).
- **Pion's own lines** reach the log through 04's bridge (04 §10: Pion's levels kept, its debug and info lines only
  while debug is on, 20 lines a minute per scope, no SDP-like line), under `component=sfu` with the scope, and the
  SFU's logger factory (`pionlog.go`) changes two things on top: Pion's info lines are debug lines (they repeat,
  without ids, what the SFU says itself), and so are its warnings and errors that hold an IP address (a check from a
  stray address names it). Pion's other warnings and errors keep their level. The group 11 integration made netx's
  Pion logger send Pion's info lines to debug as well (04 §7.3), so that at info and above no line, Pion's
  included, names an address (README §6 "Decided after group 11" (2)).
- **Tests**: `TestMetricsAfterLayerSwitches` (every metric after integration 2, and `Snapshot` and both Conns'
  `Stats` of the same session), `TestLogsHoldNoSDPOrAddresses` (the log capture test: a session with every kind of
  SFU line, on the ICE-TCP transports too and with a probe, logged at debug and at info; it fails on SDP, a
  candidate or an IP address in any component's line at info and above), and in `stats_test.go`
  `TestSnapshotGolden` (`testdata/golden/snapshot.json`, with exact rates from a hand-fed media path and the test's
  clock), `TestTotalsOutliveTheirObjects` (every total wired by hand driven to a value of its own, and kept after
  the share ends), `TestDownTrackTick`, `TestPionLogs`, `TestHoldsAddress` and `TestDowngradeReasons`;
  integrations 4 and 19 also read the repair and duplicate totals from `Metrics` (`NACKReceived`, `NACKServed` and
  the egress `rtx` packets at least what the DownTracks counted, with some egress `rtx` bytes; `IngressDuplicates`
  at least what the layers counted), and integrations 12 and 13 read `codec_mismatch`, `decoder_unavailable` and
  `decoder_failed`.

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
| Opus | A committed 2 s asset (101 packets, about 20 KB), generated once with `opusenc` (opus-tools and libopus, BSD) by `testdata/gen-opus.sh` plus a pure-Go Ogg page reader; the source loops its second second (packets 50–99), which has a full second of encoder state behind it, so the loop point is seamless. CI never needs opus-tools. There is no pure-Go Opus encoder |

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
  - Default 640×360@30 for `f` and 320×180@15 for `q`. About 2.3 Mbps for `f` with the default 3 s GOP (about
    4 Mbps with a 1 s GOP), dominated by the I_PCM IDR. Browsers decode it everywhere.
- **Opus**: the asset loops in both modes (a 440 Hz bed with whole cycles per second, so the loop is
  phase-continuous, plus a 1 kHz beep in the first 100 ms of each second; `gen-opus.sh` shifts the tone by libopus's
  312-sample pre-skip). The beep starts at loop packet 0, in sync with the flash frame.

`internal/client/publish` (M1 subset): `Publisher{PC, Source}` creates one video TrackLocal per rid, stamping the
mid/rid extensions itself (Pion's built-in tracks don't for simulcast senders, as S4 found), plus one audio track. It
packetizes with `pion/rtp/codecs` (`H264Payloader`, MTU 1200; Opus as is), computes `rtp_ts = base + (capture −
t0) × clock`, and sends its own SRs every 1 s per SSRC (NTP = wall clock at capture, RTP = that frame's timestamp,
exactly the plan's native path). It answers PLI with `Source.RequestKeyframe` and registers the TWCC header-extension
interceptor, so the SFU's feedback generator does real work. No rate control in M1. Two test options send what
browsers send and fake media alone doesn't (README S63, `repair.go` and `track.go`):
- **`RedundantRTX`** (§17 test 19) makes the Publisher repair like a browser. It negotiates RTX for the video
  (PT 97, `apt=96`) and registers **no** NACK responder: the tracks answer NACKs themselves, from a ring of the
  last 1024 packets each sent, because the RTX stream has one sequence of numbers, which the second copies below
  must share with the repairs. Video goes out on the RTX stream (RFC 4588: RTX SSRC and PT, the stream's next
  sequence number, the original's timestamp and marker, the original sequence number in front of the payload),
  with `repaired-rtp-stream-id` naming the layer when the video is simulcast; audio is the packet again. After its
  first 15 frames (a receiver can place a simulcast layer's RTX stream only once that layer's media has arrived)
  a video track also sends a second copy of each frame's last packet on the RTX stream right after the frame, the
  way Chrome fills its padding and probes with payloads it already delivered. A receiver must drop those copies.
- **`Padding`** (§17 test 6): after every `Padding`-th frame of each video layer the Publisher sends two
  padding-only packets (200 bytes of padding, no payload, the frame's timestamp) on the layer's media stream, as
  a browser without RTX pads and probes. They take sequence numbers and carry nothing, so a receiver that forwards
  the stream must close the gaps they would leave. 0 sends none; a negative value is `ErrInvalidOptions`.
- `TrackStats` counts them apart: `Padding`, `Redundant` (second copies) and `Retransmitted` (NACK answers, with
  `RedundantRTX` only; without it Pion's responder answers and nothing counts there). `Packets` and `Octets` are
  the media stream's alone. `ErrNotImplemented` is gone with the last option that returned it.

`sfutest.Viewer`: a Pion subscriber (NACKs, RR, and REMB either from the `rembsim` model driven by a scripted
link capacity (§17) or fixed values injected by the test) that records per track: seq, ts, arrival, marker contents
(layer, frame, flash), gaps, recovered-by-RTX, final loss after 1 s, keyframes, SRs. It offers the S4 checks
(`CheckContinuous`, `CheckStartsOnSPS`) plus `AVOffset()` (flash vs beep, both mapped to NTP through the last
forwarded SRs).

**The scripted link and its REMBs, as README S84 built them** (`sfutest/link.go`, `faultconn.go`, `viewer.go`):
- **`sfutest.Link`** is the bottleneck of one client's downlink: `NewLink(capacity)`, `SetCapacity` while a test
  runs, `Counts()` (RTP bytes sent through it and passed, datagrams lost). `FaultConn.SetLink(peer, link)` puts it
  in front of a client's address on the SFU's socket (`HarnessOptions.FaultyUDP`): every datagram to that client
  takes its size from a buffer that fills at the capacity and holds 100 ms of it (`LinkBuffer`). An RTP datagram
  that finds too little there is lost; everything else (STUN, DTLS, RTCP) always passes but takes its share, so ICE
  and the reports go on as they would beside a congested media stream. This is the "rate limit added for this" of
  §17's `rembsim`.
- **`ViewerOptions.REMB`** (a `*Link`) makes the Viewer send REMBs as Chrome's estimator would on that link: every
  50 ms it steps `rembsim`'s model with what the SFU sent through the Link and what passed, and sends the REMBs the
  model sends (the first estimate, then every second, and at once on a fall of 3 %), naming every video SSRC it
  receives; `Viewer.REMBs()` lists them. Without it a REMB goes out only when the test sends one (`SendREMB`).

**The Viewer's NACKs are its own** (README S63), not Pion's generator, which never sees what Pion's receiver
recovered from an RTX stream and would ask for it again every interval for as long as its window holds the
sequence number: no browser does that, and an SFU that answers NACKs would send every repaired packet several
times. Each track's Recorder knows which sequence numbers are missing, and a loop per track sends NACKs for them,
timed about as a browser's: once a number has been missing for `NACKAfter` (10 ms; a packet that is only out of
order needs none), again every `NACKEvery` (100 ms) while it stays missing, at most `NACKTries` (10) times, never
once it counts as lost (`FinalLossAfter`), and never again once it arrives, on the track or through RTX. A NACK
packet names at most 256 sequence numbers; more go into the next one. `ViewerOptions.NoNACK` asks for nothing.
The Recorder also keeps a hash of each payload, so a test can compare what arrived with what the publisher sent,
and the NACKs it sent. `HarnessOptions.FaultyUDP` puts the Harness's SFU socket behind a `FaultConn`
(`Harness.Wire`), whose filter gets the client's address with every datagram, so a test can lose chosen packets
between the SFU and one client (integration 4).

**H.264 profiles and decoders in the test tools** (README S69, for §8.3–8.5 and integration 12 and 13):
- **`publish.Options.Profile`** (default `6400`) **and `Options.Profiles`** (at most 4 more): the Publisher offers
  every profile it can send, `Profile` first, each on a payload type of its own with its RTX right after it, and
  sends the first one the answer lists, as a browser does, so the room's codec policy chooses. On a codec hint the
  caller does what a browser sharer does (01 §8.10): `Publisher.SetProfile` puts the hinted profile first, and the
  next offer's answer has the Publisher switch. The switch is made in the send loop, between two packets
  (`applyProfile`), each layer from its next keyframe, which the Publisher asks the Source for; a switch made when
  the answer arrived raced with a keyframe the Source had made in the old profile already. A Source that is a
  `publish.ProfileSource` (one that can encode another profile; in M2 the native engine re-initializes its encoder)
  is switched first; any other Source keeps its SPS, and only the payload type changes.
- **`sfutest.ProfileSource`** is a `publish.ProfileSource` made of `fake.Source`s, one per profile, on one timeline:
  a switch makes a new Source in the new profile, whose capture times go on from the moment of the switch and
  which starts with a keyframe of every layer.
- **`sfutest.ViewerOptions.H264`** lists the profile-level-ids a Viewer decodes (by default the five of §8.1), and
  `NoH264` registers no H.264 at all, like a fresh Firefox before its decoder download. `Viewer.SetH264` changes
  them for the PeerConnections the Viewer builds from then on (`Rebuild`), while its current one keeps what it
  negotiated: a browser uses a downloaded decoder on a new PeerConnection and may not on the one it has.
- **`export_test.go`** has `AdvanceCodecClock`, `CheckCodecPolicies`, `Timings.CodecRetry`, `CodecRetries`,
  `CodecPolicyHold` and `Conn.Decoders` (the guard's state: H.264 in the caps, refused, rebuilds, failed, a retry
  set), §17.

**Layer pausing and the admin's cap in the test tools** (README S88, for §11):
- **`publish.Publisher.SetActive(layer, active)`** turns a video layer off and on again, as a browser sharer does with
  the `active` flag of a quality hint's encodings (01 §8.10). An inactive layer sends nothing (its Source packets are
  dropped and counted in `TrackStats.Paused`) and writes no sender reports, while its RTCP is still read; turned on
  again it asks the Source for a keyframe and sends nothing before one, as an encoder starts an encoding it turns
  back on. Turning a layer to the state it has changes nothing; a layer the Publisher doesn't send is
  `ErrInvalidOptions`. M1's Publisher still has no rate control: the SFU's REMBs are only counted
  (`TrackStats.REMBs`, and `LastREMB` the bitrate of the last one naming the SSRC).
- **`sfutest.ApplyHint(pub, ev)`** does with a `QualityHintEvent`'s encodings what a browser does with
  `setParameters`: each layer that an encoding names by its rid is turned on or off (`SetActive`); an encoding of a
  layer the Publisher doesn't send (M5's `h`) is passed over, and a hint without encodings changes nothing.
- **`export_test.go`** has `PauseAfter` (10 s), `REMBLifts` (3) and `REMBUncapped` (1 Gbit/s), `SFU.AdvanceUplinkClock`
  (the uplink policies' clock is `monoNow` plus a skew, `SFU.uplinkSkew`, like the allocator's and the codec
  policy's), `Conn.CheckUplink` (the look the ticker has the actor take once a second, at once) and `Conn.Uplink`
  (a share's `UplinkState`: the cap the sharer was told, the paused rids, the last hint's reason).

### 15.2 `cmd/isshoni-loadtest`

```
isshoni-loadtest -server https://example.org -admin admin   # password from ISSHONI_LOADTEST_PASSWORD or a prompt
  [-scenario focus|all-high|smoke]  [-publishers 10] [-subscribers 10] [-duration 10m] [-warmup 60s]
  [-full 1920x1080@60:8M] [-preview 640x360@15:300k] [-gop 3s] [-profile 6400|42e0] [-decodable]
  [-focus-rotate 30s] [-login-interval 3s] [-json out.json] [-keep-users] [-v]
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
- **Scenario `smoke`** (CI): 2×2, 30 s, `-decodable`, the same checks with relaxed thresholds. Explicit
  `-publishers` and `-subscribers` override a scenario's size. With `-subscribers 0` the tool only publishes for
  `-duration` and skips the client checks, so a browser can be the viewer (`web/e2e/decodable.spec.ts`, README S89,
  §17).
- **Server data**: every 5 s, `GET /api/v1/admin/dashboard` (04 §11.4: `server.process` with CPU seconds, RSS and
  NumCPU, the live rooms and shares from `Snapshot`, and totals). Once before clients join (baseline RSS) and during
  the run. The egress value is the dashboard's `media.egressBps` (the SFU's RTP-level total,
  `Snapshot.Totals.EgressBps`), not `transfer.egressBps` (socket bytes).
- **Client data**: per track from `sfutest.Viewer`; per rotation, the switch latency (update sent → first packet of
  the new layer); A/V offset per focused share.

Pass criteria for `focus` (after warm-up; exit 0 = pass, 1 = fail, 2 = setup error, 130 = interrupted, README S89):

| Check | Pass |
|---|---|
| Server CPU | process CPU ÷ (wall × NumCPU) < 50% on average (4 vCPU VPS) and < 65% in any 5 s sample |
| Loss | unrecovered loss after NACK/RTX < 0.5% of expected packets, over all subscriber tracks (pre-recovery loss is reported too) |
| PLI storms | upstream PLIs per layer ≤ 0.2/s on average, and never more than 10 in any 10 s window |
| Memory | (RSS − baseline RSS) ÷ DownTracks < 1 MB |
| Egress | `media.egressBps` (RTP level, not `transfer.egressBps`) within ±15% of the expected value (sanity check of the scenario) |
| Switching | p95 switch latency < 1 s; every switch starts on an SPS; seq continuous per track |
| A/V | p99 \|flash − beep\| ≤ 5 ms at the SFU level |

Output: a human summary plus JSON: `{"scenario","params","server":{"cpu":{avg,p95},"rssPerDownTrack","egressMbps"},
"loss":{"raw","final"},"pli":{…},"switchMs":{p50,p95,max},"avOffsetMs":{p50,p99},"pass":true,"failures":[]}`.

**As README S89 built it** (`cmd/isshoni-loadtest`: `main.go`, `auth.go`, `client.go`, `scenario.go`, `measure.go`,
`report.go`; the tests are §17's), where the text above leaves a choice or the tool goes past it:
- **Files**: besides the five of §4, `client.go` holds the simulated clients: 01's Go signaling client on `/ws` with
  the account's session cookie and client kind `tool`, a `publish.Publisher` on `internal/media/fake` (Synthetic, or
  Decodable with `-decodable`) for a publisher, an `sfutest.Viewer` for a subscriber. Against a server on this
  machine the clients use loopback UDP candidates only.
- **The command line.** `-v` logs at debug, Pion's lines included (without it the clients' Pion stacks log errors
  only: forty clients that connect at once log many harmless warnings). Explicit flags override a scenario's
  defaults. `-subscribers 0` with `all-high` is a usage error. Every layer and profile is also checked by
  `fake.New` before any account exists, so a layer above H.264 level 6.2 (`-full 8192x8192@30:8M`) is a usage
  error, exit 2, not a setup failure halfway through a run (review S89-R4). The password comes from
  `ISSHONI_LOADTEST_PASSWORD` (04 §4.2 reserves it) or a no-echo prompt on a terminal (`golang.org/x/term`, a module
  of its own that `bin/isshoni` doesn't link).
- **Exit codes.** 0 the run passed; 1 it failed: a check, a client that lost its connection, a teardown that left
  accounts behind, and **media that never came** (shares that never go live, subscriptions that never deliver,
  within 60 s each): media flow is the server's job under test; 2 a setup error: a usage error, a server that can't
  be reached, a refused login, an account, a connection, a `share.start` or a pub negotiation the server would not
  make; **130 interrupted** (SIGINT or SIGTERM): the teardown still deletes the accounts, and from the first signal
  on a second one has its default action again and quits at once, leaving them (review S89-R6).
- **`-duration` covers the whole run from the moment every client is up, the warm-up included**; the checks use only
  the part after the warm-up. So `-subscribers 0` publishes for exactly `-duration`.
- **Teardown** closes every client, deletes the run's accounts (unless `-keep-users`), revokes the invite, then
  deletes any account `lt-<run>-<n>` that the admin's user list has and the run doesn't know: one whose sign-up the
  server committed but whose answer never came (an interrupt, the 30 s request timeout, a dropped connection;
  review S89-R5). It has 60 s, and logs the admin out last. The report's `users` says how many were created and
  deleted.
- **Expected egress is computed from the publishers' measured rates**, not from the nominal `-full`/`-preview`
  bitrates: per dashboard sample, the subscriptions the subscribers want then (by share: full, preview, audio) ×
  the payload rate each publisher's Publisher counters measured for that layer and its audio. It works for
  Decodable video too, whose bitrate follows from the picture. The report adds `receivedMbps`, what the subscribers
  received after the warm-up (payload and repairs), as a check from the other end; it matched `media.egressBps`
  within about 2 % in every run.
- **The memory check counts as "nothing to measure" when the dashboard reports `rssBytes` 0**, as a server built with
  `CGO_ENABLED=0` on macOS does (its process collector reads no RSS). A check with nothing to measure is reported
  as such and is not checked; so are the subscribers' checks with `-subscribers 0`.
- **Switching, seq and ts continuous per track** (review S89-R1): each focus move measures the switch of both
  subscriptions to their new layers (`subscribe.update` sent → first packet of the new layer, within 5 s), which
  must start on a keyframe with its SPS in front. **Once repairs have had their chance** (`FinalLossAfter` + 200 ms
  after that first packet) each switch is checked again, in the packets as they stand then: the packet in front of
  the new layer's first one must have arrived (seq continuous) and the new layer's first frame must come after it
  (ts forward). A switch whose SPS packet came late or by a repair is checked then too, not counted as a lost start.
  A switch the run's end cuts short keeps what its first look found. The report's `switchMs` has `verified`,
  `seqGaps` and `tsBack`; both must be 0.
- **A/V** pairs every flash frame with its beep at each viewer, both mapped to NTP through the sender reports the SFU
  forwarded (`avPairs`, `mapNTP`), after the warm-up only; `sfutest.Viewer.AVOffset` can't leave out the warm-up and
  errs when a share's audio comes back on a second track.
- **`smoke`'s relaxed limits** (2 × 2, 30 s with a 5 s warm-up, `-decodable`): final loss < 2 %, egress 50–200 % of
  the expected, PLIs ≤ 1/s per layer and ≤ 20 in any 10 s, switch p95 < 3 s, A/V p99 ≤ 20 ms. CPU and memory are
  reported only: in CI the clients share the machine, or the process, with the server.
- **`all-high`** needs at least one subscriber. It starts as `focus` without the rotation; after the warm-up, every
  30 s it records the step that ends (full layers, egress and its expected value, average and highest CPU, loss
  before and after NACK/RTX) and raises the next 10 thumbnails, in random order, to the full layer without sound.
  **The knee** is the first step whose final (unrecovered) loss exceeds 2 % or whose average server CPU exceeds
  90 %; it ends the run, and so does a step with no thumbnail left to raise, long before `-duration`. The report
  has `steps` and `knee`; all-high has no pass criteria (report only).
- **The JSON report** (`-json FILE`, or `-json -` for stdout with the summary on stderr): CPU and loss in percent,
  RSS in bytes, rates in Mbit/s, times in ms. Beyond the shape above: `startedAt`, `measuredSeconds`; `server`'s
  `cpu.max`, `baselineRss`, `peakRss`, `downTracks`, `expectedEgressMbps`, `receivedMbps`, `samples`; `loss`'s
  `expectedPackets`, `tracks`, `duplicates`; `pli`'s `maxPerSecond`, `maxIn10s`, `total` and per layer; `switchMs`'s
  `count`, `withoutKeyframe`, `lostStart`, `verified`, `seqGaps`, `tsBack`; `avOffsetMs`'s `count` and `max`;
  `steps` and `knee` (all-high); `checks` (every criterion with its value, its limit and whether it was checked);
  `users` (`created`, `deleted`, `kept`); `resumes` (signaling connections that dropped and resumed); `setupError`.
- **`decodable.spec`** finds the tool through `LOADTEST_BIN` (05 §19.3), which CI sets to a binary its `build` job
  made with `CGO_ENABLED=0 go build -trimpath` (06 §8.2); without it the spec builds the tool with the Go on the
  `PATH`. The tool is test tooling: the release builds only `cmd/isshoni` (06).

## 16. Room for later milestones

| Later | What M1 already provides |
|---|---|
| M2 Windows app / M3 macOS app (native publishers, `RolePublisher`) | Roles; `tracks`-based share mapping; forwarded publisher SRs; `UplinkPolicy` slot; `QualityHintEvent.MaxBitrate`; REMB to publishers; High `64001f`/`640c1f` in the MediaEngine, matched without level; codec-policy re-offer path; `internal/client/publish` and `internal/media/fake` shaped like the C ABI |
| M3: 4K60 without a preview layer | `low` → `off` with `no_preview_layer`; single-layer shares |
| M4 Linux agent (`RoleAgent`) | publish-only role |
| M5 720p30 middle layer | rid `h` accepted (with the `layer.mid` feature) and resolved; add `QualityMedium` (additive on the wire) |
| M5 BWE improvements, v2 TWCC downlink | allocator interface; the sub API adds transport-cc and a GCC-style estimator; probing replaces trial upgrades |
| Egress above ~50k pps | `sendmmsg` via pion/transport batchconn behind the mux |
| VP8/VP9/AV1/HEVC | codec-keyed munger hooks (picture ID and TL0PICIDX rewriting for VP8); `ProfileKey` generalizes to a codec key |
| Optional E2EE (LiveKit-style frames) | keyframe detection must read the unencrypted NAL header bytes that format keeps |
| Embedded TURN | SettingEngine relay settings only; the model doesn't change |
| Mobile apps (`ClientMobile`) | no SFU change |

## 17. Test plan

All Go tests run with `-race`. `TestMain` runs `goleak.VerifyTestMain` (Pion goroutines allowed 2 s to settle after
`Close`). Timers (allocator, codec policy, share and PC timeouts) use an internal clock interface that tests replace
through `export_test.go`; integration tests use real time unless noted. The PC timers of README S57 (handshake,
grace, the sub offer re-send, the ICE restart spacing, the window of the PC creation limit and the stalled
threshold) are not behind a clock: each SFU keeps them as durations (`timings` in `config.go`) that a test shortens
with `SetTimings` from `export_test.go` before the first `Join`, and `TestHandshakeTimeout` checks the defaults
against the numbers of §5.3 and §12. README S69 kept both patterns for the codecs: the room codec policy's hold of
§8.3 reads a clock (`monoNow` plus a skew) that a test moves on with `AdvanceCodecClock`, and `CheckCodecPolicies`
looks at every room's policy at once instead of waiting for the ticker; the 20 s between two codec rebuilds of §8.5
is a duration like S57's (`Timings.CodecRetry` through `SetTimings`), not a clock. `TestDecoderUnavailable` checks
the defaults (20 s, `CodecRetries` = 9, `CodecPolicyHold` = 60 s).

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
- `packetcache_test`: sizing (pps → capacity, the 1024 video and 128 audio floors, grow and shrink hysteresis);
  out-of-order insert; a duplicate insert returns false and leaves the slot as it is; wraparound; 1.5 s expiry; after
  10 s of an idle 1 fps layer, the first packet of a 300-packet frame is still served 200 ms later; concurrent
  insert/get under `-race`; benchmark insert+get < 100 ns.
- `throttle_test`: 100 concurrent `requestKeyframe` → 1 PLI; another after 500 ms, not before.
  As README S52 wrote it (`throttle_test.go`, two tests on layers with a recording pub PC):
  - `TestKeyframeThrottle`: the hundred requests make one PLI, with one caller told so and 99 counted as
    throttled; the next goes out 500 ms after it and not a nanosecond before; a request for a slot the share has
    no layer in does nothing; a PLI that the pub PC refuses (not connected, closing) is spent all the same, so
    whoever waits asks again 500 ms later.
  - `TestKeyframeThrottlePerLayer`: the throttle is each layer's own. Twenty viewers who switch to the preview
    layer in the same moment, and whose DownTracks then ask again with every packet that is no keyframe, cost the
    publisher one PLI for `q` per 500 ms; a request for `f` right after goes out, because `q`'s PLI doesn't
    throttle it.
- `subscription_test` (README S52): the table of §10.1 (`TestSelectSlot`) and its reasons (`TestShortfall`); the
  events and their order (`TestSubscriptionStateEvents`, `TestSubscriptionOfPendingShare`); the pacing of §6.2
  (`TestSubscriptionEventsPaced`: a thousand notices leave one entry in a busy actor's queue, and a stream that
  alternates between two profiles, 802 changes in a few milliseconds, makes two events: that it began, and 250 ms
  later how it ended); a forwarded layer that ends (`TestForwardedLayerEnds`); and the
  transceiver reuse of §9.3 with its three corner cases (`TestSubTransceiverReuse`, `…ReuseBinding`,
  `TestSubSpareNotFreeYet`, `TestSubSpareNotInactive`).
- `rtx_test`: RTX packet layout (OSN, SSRC, PT); per-seq limits; budget rate and depth (after 10 s of an idle
  8 Mbps `f`, NACKs for 30 packets of one large frame are all served).
  As README S63 wrote it (`rtx_test.go`, on a DownTrack without a sub PC, `newRTXRig`): `TestRTXLayout`,
  `TestRTXAudio` (the packet again, counted by the next sender report), `TestNACKLimits`, `TestNACKRecordAge`
  (the 5 s of a record, to the nanosecond, and the packet that has the number 65536 packets later),
  `TestNACKMisses`, `TestNACKAcrossEpochs` (a switch and a padding gap), `TestRTXBudget`, `TestRTXRate` (the three
  maxima after `SetLimits` and a preset change), `TestNACKBatch` (the 256), `TestRTXQueue` (the 256, a rebuilt
  binding, a closed gate, no PT), `TestWriterSendsRTXFirst`, `TestSenderReportForwarding` (§9.6), and
  `TestPubNACKFilter` (§8.1; `TestInterceptors` in `codec_test` holds the filter's place in the chain).
- `allocator_test` (fake clock): scripted REMB and RR sequences → state changes, backoff doubling up to 120 s and
  reset, events once per change, allocation order (audio, thumbnails, newest high); a REMB below a target the viewer
  isn't receiving yet never downgrades; a REMB after a gap of more than 5 s is a baseline, not a decrease. Then the
  three cases of integration 16 with REMBs from `rembsim` (below) instead of scripted values, including recovery
  after the backoff has reached 120 s.
- `rembsim` (`internal/server/sfu/sfutest/rembsim`, used by `allocator_test` and `sfutest.Viewer`): a small copy of
  libwebrtc's AIMD estimator, driven by a scripted link capacity. Received rate = min(forwarded, capacity) over the
  last 0.5 s. The estimate starts at the received rate. Overuse (forwarded above capacity) sets it to 0.85 × the
  received rate when that is lower, at most once per 200 ms. Otherwise it grows by 8 %/s but never above max(itself,
  1.5 × received + 10 kbps). A REMB goes out every 1 s, and at once when the estimate falls by 3 % or more. The excess
  above capacity is lost, and the viewer's RRs report it (in integration tests `FaultConn` drops it, with a rate limit
  added for this). Its own test checks these rules against a few values worked out by hand.
- `pacer_test`: a 150 KB burst at 24 Mbps takes 50 ms ± 10%; a 1 fps idle stream followed by a 300-packet frame
  arriving at 20 Mbps adds less than 20 ms; the same frame injected all at once leaves at the configured rate
  (20 Mbps for an 8 Mbps `f`, ± 10%).

As README S84 wrote the three (`allocator_test.go`, `sfutest/rembsim`, `pacer_test.go`). The allocator's clock is
`monoNow` plus a skew (`SFU.allocSkew`, like the codec policy's), which the allocator's tests move by hand or step
past by passing the time to the state machine; for integration 16, `export_test.go` has `Conn.Downlink` (the
state, budget, backoff and cause), `BackoffStart` and `ProbeTime`:
- **`allocator_test`**: `TestAllocatorREMB` (which REMBs fall; a copy changes nothing; a fall is an overuse only
  below the forwarded rate), `TestAllocatorStates` (every row of §10.2's table, the budget each time, the backoff
  doubling to 120 s and its reset, the estimate), `TestAllocate` (cheapest first, newest `high` first, a share
  without a smaller layer never capped), `TestAllocatorRembsim` (integration 16's three cases in fake time with
  `rembsim`'s REMBs, then a throttle long enough for the backoff to reach 120 s, and its end),
  `TestAllocatorReports` (loss as the link lost it: the SFU's own gaps out, a late number counted again, the first
  report of a stream or an SSRC and one after 2 s only a start), `TestAllocatorReportFlood` (10 000 reports in a
  second keep 5 that still add up; at most 1024 kept) and `TestAllocatorEvents` (a viewer Conn with real
  DownTracks: the events the client hears, the newest request served first, a failed trial, the `bandwidth`
  downgrades and the estimate).
- **`rembsim`** has the rules above, with one the text leaves open: **overuse is judged on each step by itself**, a step
  in which the link lost more than 1 % of what was forwarded, not on the 0.5 s average. Chrome sees overuse in the delay
  of the packets that queue at the bottleneck, which stops growing as soon as the sender slows down; the model's
  equivalent is the link dropping now. `sfutest.Viewer` steps it every 50 ms (§15.1). Its tests are `TestModel` (the
  rules against hand-worked values, stepped every 100 ms) and `TestModelCeiling` (1.5 × the received rate plus 10 kbit/s
  however long the link has room, and no fall without overuse); `sfutest`'s `TestLink` and `TestFaultConnLink` check the
  Link.
- **`pacer_test`**: `TestPacerRate` (the rate and the depth), `TestPacerSchedule` (the three cases on the bucket
  alone in simulated time, exact), `TestPacerWriter` (the same through a DownTrack's writer goroutine in real time,
  within 10 %) and `TestPacerRTX` (retransmissions are paced like media and first, audio never; §9.5). **The
  150 KB burst takes 50 ms from an empty bucket**, as right after another burst, which is what the case means; from
  a full one its 60 KB leave at once and the burst takes 30 ms (§9.8). `TestPacerSchedule` checks both, and
  `TestPacerWriter` empties the bucket with a 60 KB burst first.
- **Uplink control** (README S88, `uplink_test.go` and `pausing_test.go`, §11): `TestM1UplinkPolicy` (the policy on a
  fake clock: the admin's cap always; the full layer off once nobody has asked for `high` for 10 s, not a nanosecond
  before, counted from the share's start or from the first look that finds the request gone; on again with the first
  request; never off without a preview layer), `TestUplinkHints` (a hint when the cap changes, with the reason
  `admin`, and when the pause state does, `viewers`; both in one hint with `viewers`; a request that ended 0.9 s after
  a look is not paused 9.1 s later; a paused layer's keyframe requests do nothing, so the one after the resume finds
  the throttle unspent; a viewer held at the preview layer by its allocator still counts as asking for `high`;
  `UpdateShare` and `Resync` say the same), `TestUplinkUpdateShare` (an `UpdateShare` between a new cap and the look
  `SetLimits` posts returns the new cap, and the look still sends the `admin` hint), `TestAllocatorPausedLayer` (a
  `high` for a paused share in `limited` is capped with `bandwidth` when the full layer's maximum doesn't fit, still
  resumes it, and weighs the maximum for 2 s after the resume), `TestUplinkREMB` (one REMB per look while every
  share of the pub PC has a cap: the sum, naming the video SSRCs, paused ones included, sorted; none while one share
  has no cap or the PC is away; three of 1 Gbit/s once the cap goes, then none; a new pub PC is owed none),
  `TestLayerJitter` (§9.1: 0 for packets on time, |D| for a delay that alternates by D, late packets and a pause of
  more than 10 s change nothing, a frame of many packets is one sample), and the three on real PCs:
  `TestAdminCapREMB` (the slice's acceptance: with `MaxShareKbps` set the pub PC gets the REMB every second, naming
  its video layers; a new cap reaches the sharer at once as an `admin` hint and the next REMB; when the cap goes the
  hint says so, then three REMBs of no limit, then none), `TestLayerPausing` (nobody asks for `high` for 10 s, and
  the sharer, which applies the hint as a browser would through `sfutest.ApplyHint`, pauses the full layer; the
  share stays live with both layers listed; a WebSocket resume brings the hint again; a request for `high` resumes
  it at once with a PLI, and the viewer's stream goes on in the full layer from its keyframe; back on the preview
  layer it is paused again 10 s after the first look that finds the request gone) and `TestLayerPausingOff`
  (`sfu.pause_unwatched_layers = false`: no hint for an hour, and the full layer keeps flowing). `core_test`'s
  `TestShareParams` has the paused encodings, `surface_test` the new types and the two reasons, `publish`'s
  `TestSetActive` the Publisher's side and `sfutest`'s `TestApplyHint` the hint's.
- `sdpcheck_test`: each validation code; a sending m-section without a binding, or bound to a share that isn't this
  Conn's, carries no share and is no error (and skips the per-share checks, the kind check included); the Opus edit
  per m-line (stereo and `maxaveragebitrate`) and `a=inactive` on the m-lines without a share; the answer check for
  m-lines without H.264. README S69 added `TestPubAnswerCodecFilter` (step 3 of §8.4 on Chrome's offer, through a
  Conn: the answer's video holds the policy's profiles with their RTX in the order of the offer it answers; a
  re-offer with the hinted profile first gets it first; back at `high` a re-offer gets every profile again, although
  the transceiver's preferences were filtered before; an offer without an allowed profile is answered with what it
  has) and `TestInOfferOrder` (codecs in the offer's order by payload type, what either side lacks left out), and
  `FuzzSDPCheck` runs `inOfferOrder` on every m-section it parses (each codec of the list at most once, and nothing
  that isn't in the list).
- `policy_test` (fake clock): high → cb at once; cb → high after 60 s; conns without H.264 ignored.
  As README S69 wrote it: `TestCodecPolicyNext` drives the hysteresis by hand, and `TestRoomCodecPolicy` runs it
  through an SFU (the viewer set, in which Conns without H.264 and Conns that don't subscribe are no viewers; the
  reports of a change to the hub and, per share, to the publishing Conn; `ShareParams.Profile`; the hold on the
  fake clock; `SetDecodeCaps` and `Resync`).
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
   As README S63 wrote it (`repair_test.go`): the SFU's socket is the Harness's `FaultConn`
   (`HarnessOptions.FaultyUDP`), and the viewer that loses packets has a socket of its own, so the filter tells it
   from the second viewer of the same share by its address. For about 5 s it loses single video packets and runs
   of three, and single audio packets (at least 10 of each kind). Every gap closes (video through the RTX stream only, audio through the
   packet itself), nothing is lost for good, each frame and audio packet is the publisher's by a hash of its
   payload, nothing arrives twice on the media stream, the DownTrack's repair counters show no miss and no queue
   drop, and the sub PC's round-trip time has been read: the test wants a value above 0 and under a second, no
   tighter (a loaded machine stretches one STUN round trip on loopback a long way). The second viewer sees no gap
   and sends no NACK. Since README S79 the SFU's `Metrics` must show at least the NACKs and retransmissions that the
   DownTracks counted (`NACKReceived`, `NACKServed`, egress `rtx` packets), and some egress `rtx` bytes.
   **Since README S84 it loses 5 % of the video, not 10 %** (sequence numbers 7 and 40–42 modulo 80, still at
   least 10 video packets). Pion's receiver reports, like Chrome's, count a packet that came back on the RTX
   stream as lost, so 10 % lost and repaired is congestion by §10.2, and the downlink allocator rightly moved that
   viewer to `q`, which broke a test about repair. At 5 % the reports stay under the 10 %.
5. `TestSRTranslationAndAVOffset`: |flash − beep| ≤ 5 ms after 10 layer switches and 5 audio off/on cycles; forwarded
   SR RTP equals upstream RTP + offset.
6. `TestPaddingNoGaps`: the publisher injects padding-only packets; the viewer sees no seq gaps and sends no NACKs.
7. `TestSubICERestart`: black-hole the viewer's UDP for 5 s; the viewer calls `RestartICE`; media resumes within 3 s
   of unblocking on the same SSRC; the share never leaves `live`. Resume case: the viewer's signaling drops while its
   UDP is black-holed; after 3 s, `Resync()` and then the viewer's `RestartICE` arrive: exactly one ICE-restart offer
   is sent, and media resumes within 3 s of unblocking.
   As README S57 timed it: the request and the network come back together. After the 5 s the viewer calls
   `RestartICE` and its UDP is unblocked in the same moment, as for a client whose network has just returned; the
   3 s run from the unblocking to the first packet of the resumed video, which starts on a keyframe, on the tracks
   the viewer had. In the resume case the outage lasts until the SFU itself sees the sub PC `disconnected` (Pion's
   5 s of §7.3, so about 5 s rather than 3) and at least 3 s; then `Resync()`, the viewer's `RestartICE` and the
   unblocking follow one another at once. `Resync()` starts the restart and the viewer's request finds it under
   way (§5.3), so the test sees three sub offers in all and none sent into the dead signaling.
   S57 timed it so because with Pion's 7 checks per pair, a restart asked for while the viewer's network was still
   away gave each pair up about 1.5 s after its answer and never connected. Since README F10 the sub PCs check a
   pair for the length of the failed timeout (§7.3), so the request may now come during the outage, as a client's
   does 3 s after its ICE noticed (01 §11.5 B); 01's `TestUDPBlackHoleViewer` (01 §19 case 5) asks that way.
   `TestSubICERestart` keeps S57's timing.
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
    As README S69 wrote 12 and 13 (`firefox_test.go`, with `sfutest.Viewer`'s `H264`, `NoH264` and `SetH264`, and
    `sfutest.ProfileSource`, §15.1):
    - `TestCodecPolicy`: a sharer sends High to a Chrome viewer; a viewer that decodes only CB joins, and the policy
      is `cb` at once; the sharer gets `CodecPolicyEvent{42e0}`, the new viewer hears `codec_mismatch` while the
      share is still High, the sharer re-offers with `42e0` first and the answer holds only `42e0` and `4200`; both
      viewers then get the video on their CB payload type, the Chrome viewer without a gap in its stream. When the
      CB viewer has left, the policy is `high` again 60 s later on the fake clock, and so is the sharer.
    - `TestDecoderUnavailable`, with the 20 s shortened to 300 ms (`SetTimings`), in three parts. Caps-driven: no
      video m-section and `decoder_unavailable` while the caps list no H.264, the audio flows, and caps that gain it
      rebuild the sub PC once (`gen` 2) with the video. The guard: caps that list H.264 with a MediaEngine that has
      none; the answer is applied without an error, 9 rebuilds follow one per interval, then `decoder_failed` and no
      more rebuilds; the same caps again change nothing; the SFU's rebuilds don't count toward the PC creation limit
      (a `ResetPC` after them still gets a PC, which doesn't restart the guard); caps that change after the
      decoder download rebuild at once and the video comes. A decoder between shares: a viewer that gains H.264
      while it watches nothing gets no rebuild then, and its next subscription gets a new sub PC with video.
    - `TestDecoderCapsOfClosedSubPC`: caps that gain and lose H.264 21 times on a closed sub PC build no PC; the
      client's next request (`Resync()`) builds the successor, which carries the video.
14. `TestHandshakeTimeout`: a client that never completes ICE → PC closed at 10 s ± 1 s, `PCStateEvent`. An ICE
    restart that never completes gets no server timer and no event.
    The 10 s are measured from the PC's first answer (§5.3), for a pub and a sub PC, in real time; a sub PC whose
    offer has no answer yet has no timer running. `TestHandshakeTimeoutShort` runs the timeout with a short timer.
15. `TestGuards`: a third PC, a fifth share, a bad rid (`h`, or three rids), a malformed track binding (a video
    m-line bound as audio), no H.264, a data channel, a stale pub offer → the §6.3 codes, with nothing applied. A share
    stopped while its re-offer is in flight (the offer still binds it): the answer comes back with that share's
    m-lines `a=inactive`, no error, and the publisher's other share keeps flowing.
16. `TestDownlinkAdaptation`: the viewer's REMB comes from `rembsim` with a scripted link capacity, starting at
    20 Mbps. (a) Idle → motion: the focused share's `f` runs at 0.2 Mbps for 30 s (`Source.SetBitrate`), then at
    8 Mbps for 30 s; the viewer stays on `high` and gets no `bandwidth` state event. (b) Throttle: capacity drops to
    3 Mbps while `f` runs at 8 Mbps; within 3 s the focused share is forwarded `low` with reason `bandwidth`, and the
    first trial upgrade fails (on loss, §10.2). (c) Recovery: capacity returns to 20 Mbps; the viewer is back on `high` within one
    backoff period (here 20 s) plus the 6 s probe, with no action from the test.
    As README S84 wrote it (`adaptation_test.go`, with `FaultyUDP`): the bottleneck is an `sfutest.Link` on the SFU's
    `FaultConn` in front of the viewer's own socket, and the viewer's REMBs are `rembsim`'s model of that link
    (`ViewerOptions.REMB`, §15.1); the publisher sends the Auto preset with audio, its GOP longer than the test. (a)
    also checks that the last REMB stayed below 8 Mbps (the case is about a REMB below the forwarded rate) and that
    the allocator is in `normal`. In (b) the first trial **fails on a falling REMB in every run, not on loss**: with
    `rembsim`'s rules the estimate never falls without overuse, so it is still at the throttle's level when the
    trial begins, and the trial's overuse cuts it within about 250 ms, before a receiver report shows the loss
    (§10.2). The test wants the trial to fail on loss or on REMB, logs which, and wants the backoff at 20 s after
    it. (c) wants `high` within the 20 s backoff plus the 6 s probe, and the allocator back in `normal` within 30 s:
    those 26 s, up to 1 s for each of the two ticker looks that end the backoff and the trial, and 2 s of room for
    a loaded machine. At the end `isshoni_sfu_downgrades_total{bandwidth}` counts at least the cap and the failed
    trial.
17. `TestTenByTen` (skipped with `-short`, S4 port): 10 publishers × 10 viewers; all 100 subscriptions on the right
    layer within 5 s; audio only on focused shares; integrity checks; heap growth per DownTrack < 1 MB.
18. `TestProbe`: a Pion data-channel offer per transport (`udp`, `tcp443`, `tcp7882`) → connected; the answer holds
    only that transport's candidates; data-channel messages come back unchanged; a second probe of the same user and
    transport closes the first; a 21st probe on the server → `sfu.probe_limit`; each transport without its mux →
    `sfu.transport_disabled`.
19. `TestRTXDuplicates`: the publisher (test option `RedundantRTX` of `publish.Publisher`) resends packets it
    already delivered on its RTX stream, as Chrome does for padding and probes; each viewer receives every seq exactly
    once and sends no extra NACKs; `isshoni_sfu_ingress_duplicates_total` counts the copies. A packet dropped on the
    publish side and then recovered by RTX still reaches every viewer once.
    As README S63 wrote it (`repair_test.go`; the metric came with README S79, so the test reads the same count from
    `SFU.Ingress`, `LayerState.Duplicates`, and since README S79 also checks that `Metrics.IngressDuplicates` is at
    least the layers' sum after the loss): two viewers, one on `f` and one on `q`, get every sequence number once in
    whole frames while the publisher sends a second copy of each frame's last packet on its RTX stream (at least 60
    on `f`, 30 on `q`), and the SFU counts every copy. Then the publisher's own socket (a `FaultConn` under its UDP
    mux) loses at least 20 packets that no second copy covers. Only the SFU's NACKs bring them back, on the same RTX
    stream, and the requests for each stop when it is back, because the pub PC's NACK filter takes out what the
    cache holds (§8.1, §9.5): the test wants at most three resends per lost packet on average, room for a loaded
    machine (on loopback it is one each: 20 resends for 20 lost packets, 200 before the filter). Every gap closes
    for both viewers, and what the SFU forwards arrives once; a copy on the viewer's RTX stream can only be the
    answer to that viewer's own NACK.

**The frame checks and a layer switch** (README S52, `sfutest/checks.go`). An SFU switches on the first packet of
the new layer's keyframe (§9.4), and the two layers are read by different goroutines, so that packet can reach a
viewer's queue between two packets of the old layer's current frame. The rest of that frame is then no longer
forwarded, and a decoder drops what it got of it. That is correct, and the checks say so:
- `CheckContinuous` and `CheckStartsOnSPS` see every packet, the cut frame's included: the viewer's sequence
  numbers have no gap, and every change of layer starts on an SPS.
- The checks of whole frames (`CheckVideoMarkers`, `Frames`) run on `sfutest.WithoutCutFrames(pkts)`, which leaves
  out exactly the frames that a switch cut short: a frame whose last packet (the marker bit) didn't arrive, right
  before a frame of another layer. A frame that is short for any other reason still fails. `WithoutPartialTail`
  does the same for the one frame the recording itself may have cut at its end.

**The checks and repair** (README S63, and the group 8 integration's commit "tests that hold on a loaded Linux
machine"). With repair a packet can arrive after newer ones: a retransmission that a NACK brought back, or a
packet the network or a loaded machine delayed. A decoder's jitter buffer puts such a packet back in its place,
and the checks do the same:
- **The checks of the stream read packets in sequence-number order**, the order the SFU sent them in:
  `CheckContinuous` sorts them itself (the S4 check always did), and `CheckStartsOnSPS`, `CheckVideoMarkers` and
  `CheckAudioMarkers` read them through `sfutest.InSeqOrder`, which keeps a second copy of a number right behind
  the first. (S63 added `InSeqOrder` for the tests with repair; the three checks took arrival order until the
  integration found them failing on a loaded Linux machine, where a repaired packet arrived after the next frame
  had begun.)
- `Frames`, `WithoutPartialTail` and `WithoutCutFrames` still take packets in arrival order, which is the stream's
  order as long as nothing was lost or reordered; a test with repairs puts the packets in order with `InSeqOrder`
  before it calls them.

The media integration tests of 01 (`internal/server/itest`) sign up their accounts through REST like the web app.
On a fast machine they come faster than the auth-hash budget of 03 §7.3 refills (a burst of 20), and the server
answers 503 `server_busy` with a `Retry-After`. The harness retries such a sign-up after the `Retry-After`, as it
already did for a 429 (`world.post`), within the test's deadline; any other status still fails the test at once.

Integration 2, 3, 10 and 17 are `simulcast_test.go`, with two more of README S52's next to them:
`TestSimulcastFromTheFirstPacket` (§10.1: viewers who were subscribed before a simulcast publisher sent anything
get each layer from its first packet, and the publisher is asked for no keyframe) and `TestSimulcastReoffer` (a
simulcast publisher offers again in the same `gen`: the answer is as before, no layer is attached anew, no
keyframe is asked for, and the viewer's stream has no gap). For the second, `publish.Publisher`'s offer no longer
repeats the last answer's rids as receive rids, which the SFU refuses as `sfu.bad_rid`.

**Browser e2e (owned by 05, relies on this doc)**: Chrome only (Playwright with Google Chrome, 05 §19.3). Chrome
sharer (canvas) → SFU → Chrome viewers: `framesDecoded > 0`, audio energy, switches. No browser spec measures the A/V
offset (the tone tab has no flash/beep marker, 05 §19.3): the 45 ms A/V target is checked by integration 5 (README
S63, |flash − beep| ≤ 5 ms through the SFU) and by the exit survey ("nobody reports audio out of sync", 06 §12.3).
The Go decodable publisher → Chrome check belongs to the load-tool slice (README S89, slice 15 here), not to 05's
slices: `web/e2e/decodable.spec.ts` runs `isshoni-loadtest -scenario smoke -decodable -publishers 1 -subscribers 0`
(§15.2) against 05's e2e server fixture, and a Chrome viewer in the same room shows `framesDecoded > 0` on that
share. Firefox is covered by integration 13 (a viewer MediaEngine without
H.264) and the manual matrix row M-FF-1 (05 §19.4); the codec policy by integration 12. There is no Firefox
Playwright project. The manual matrix (05 §19.4) also has the downlink row: a viewer on a throttled link (Network Link
Conditioner or `tc`) drops to `low` with reason `bandwidth` and returns to `high` within about 2 minutes of lifting the
throttle, with no reload.

**Load (M1)**: `isshoni-loadtest -scenario focus` on a 4 vCPU VPS from a second VM, pass criteria in §15.2 (a manual
milestone gate before the M1 exit test). `-scenario smoke` runs as a Go test in `test-go` against an in-process server
(S89). As README S89 wrote the tool's tests (`cmd/isshoni-loadtest`, each against an in-process server, with goleak
in `TestMain`): `TestSmoke` (`-scenario smoke` exits 0 with a JSON report that passes, and the accounts it made from
its invite are gone afterwards), `TestFocusRotation` (a short `focus` run whose subscribers move their focus every
3 s: every switch measured, on a keyframe, with seq and ts continuous), `TestAllHighSteps` (steps of one thumbnail
each, ending when nothing is left to raise), `TestInterrupted` (exit 130, the accounts deleted all the same),
`TestLostSignUpAnswer` (an account whose sign-up answer never came is deleted too), `TestUnreachableServer` and
`TestWrongPassword` (exit 2), `TestUsage` and `TestFlagDefaults`; and the analysis on its own in `measure_test.go`
(`TestParseLayer` with `FuzzParseLayer`, the percentiles, CPU, PLI windows, rates and expected egress, loss totals,
`TestFindSwitch`, `TestCheckSwitch` (the second look after repairs: a seq gap at the layer change, an SPS packet
that came late, a timestamp that doesn't move forward), the timestamps and the A/V pairs' NTP mapping).

## 18. Interfaces other docs rely on

- **Package `internal/server/sfu`** (§6.1–6.3): `New`, `Config` (with `Transport *netx.Transport`), `Deps`, `Limits`,
  `SFU` (`Close`, which returns within 1 s and never closes the Transport, `Ready`, `Join`, `Shares`, `Share`,
  `CodecPolicy`, `StopShare`, `CloseRoom`, `SetLimits`, `Snapshot`, `Metrics`, `Probe`), `Conn` (every method in §6.1;
  every PC method takes `gen`), `JoinParams`, `StartShareParams`, `ShareParams`, `EncodingParams`, `TrackBinding`,
  `ShareUpdate`, `SubscriptionUpdate`, `DecodeCaps`, `ProfileKey`, `Role`, `ClientKind`, `PCKind`, `Quality`,
  `Preset`, `SourceKind`, `ShareState`, `EndReason`, `SubReason`, `ShareInfo`, `LayerInfo`, `Signaler`,
  `RoomEvents`, `Event` and its types (`SubscriptionStateEvent`, `CodecPolicyEvent`, `QualityHintEvent`,
  `PCStateEvent`, `ErrorEvent`), `Error` (with `Share` and `RetryAfter`; `Is`) and the `sfu.*` codes
  (`Code…` constants), `ConnStats` and its types (`PCStats`, `OwnShareStats`,
  `SubscriptionStats`, `DownTrackStats`),
  `Snapshot` and its types (`RoomSnapshot`, `ShareSnapshot`, `ViewerCounts`, `ConnSummary`, `SnapshotTotals`),
  `Metrics` and its label values (§13), `ProbeTransport` (with `ProbeUDP`, `ProbeTCP443`, `ProbeTCP7882`),
  `ProbeResult` and its `Err` values `ProbeErrTimeout` and `ProbeErrClosed` (§7.6, README S76), and the uplink
  control of §11 (README S88): `UplinkPolicy`, `ShareUplinkStats`, `LayerUplinkStats`, `UplinkDecision`,
  `HintReasonAdmin` and `HintReasonViewers`. Later: `RoomLimits`, `SetRoomLimits`.
  `Role`, `ClientKind` and `PCKind` have no valid zero value (§6.1).
- **Wire-visible conventions** (through 01's `sfuplane`): rids `f`/`q`, at most 2, or none (`h` rejected until M5);
  share ids from the hub; sub msid = share id and track ids `v-`/`a-`+share id (debugging aid; mapping is by
  `tracks`); transceivers are reused from README S52 on (a `track` event fires again on reuse; before that every
  subscription gets new m-sections, §9.3); the server sends complete SDP and never
  trickles; `gen`/`neg` on offers and answers; the publish answer's Opus `maxaveragebitrate` follows the preset; the
  sub offer's Opus has `stereo=1` (the viewer must add `stereo=1` to its answer).
- **Static PT table** (§8.1). **Codec policy** semantics (§8.3). **Encodings per preset** (§8.6).
- **Config key** `sfu.pause_unwatched_layers` (registered in 04's registry).
- **Metric names** (§13).
- **`internal/media/fake`** (`Source`, `Packet`, `Config`, `Mode`) for M2's fake engine; **`internal/client/publish`**
  (`Publisher`) as the start of M2's client core; **`internal/server/sfu/sfutest`** (`Harness`, `DirectSignaler`,
  `Viewer`, `FaultConn`) for 01's integration tests in `internal/server/itest`.
- **`cmd/isshoni-loadtest`** CLI and exit codes (§15.2: 0, 1, 2 and 130) for the `smoke` Go test in `test-go`
  against an in-process server (S89), `web/e2e/decodable.spec.ts` (S89, §17; the binary through `LOADTEST_BIN`,
  05 §19.3, 06 §8.2), README S92's load gate and the exit-test pre-flight.

## 19. Depends on

**01-protocol.md**
- The wire format and `internal/server/sfuplane` (01 §15.4), which calls this package exactly as §6.4 shows: share
  ids in `StartShareParams.ID`, `gen`/`neg` and `tracks` on offers and answers, `pc.restart` → `RestartICE`/`ResetPC`,
  `pc.close` → `ClosePC` (each with the message's `gen`), caps as `h264/<4 hex>` CodecKeys (converted to
  `ProfileKey`). The hub keeps no PC counters: the SFU owns `gen`/`neg` for both PC kinds (01 §21).
- Session semantics: signal keeps one `Signaler` per Connection across WebSocket resumes, calls `Resync()` on resume,
  and after 30 s ends the Conn's shares (`StopShare` with its reason) and then calls `Close`; the hub owns share ids,
  share limits, the share lifecycle and its 30 s timeouts, and always calls the publishing Conn to stop a share.
- A Go signaling client (`internal/client/signal`, 01 §15.3) that the load test imports.
- 01 §10.4: a sub offer with a new `ice-ufrag` counts as the client's sub ICE restart, whoever asked for it (the
  client's `pc.restart{ice}` or the server's `Resync()`); the 15 s rebuild timer runs from that offer (§5.3).

**03-accounts-and-store.md**
- User and room IDs as strings (12-char lowercase base32; the default room is `lounge`).
- The setting `maxShareBitrateKbps` (pushed through `SetLimits` by the wiring).
- For the load test: admin login (`POST /api/v1/auth/login`), `POST /api/v1/invites` with `maxUses` up to 1000,
  `POST /api/v1/auth/register` with the invite token (it returns the session cookie, so no second login), and
  `DELETE /api/v1/admin/users/{id}`. Invite registrations don't consume 03's `register-ip` bucket; the load test still
  paces registrations at `-login-interval` (default 3 s) for the `auth-ip` bucket (burst 20, 1 per 15 s).
- Room deletion reaches the SFU through the hub (`Conn.Close`), not directly (`SFU.CloseRoom`, which also closes
  the room's Conns, is for callers without a hub, §6.1).

**04-server-platform.md**
- `netx.Transport` (04 §7.3) with `UDPMux` (nil if `listen.ice_udp` = ""), `TCPMux` (combined), `TCPMux443` (nil in
  off mode), `TCPMux7882` (nil if `listen.ice_tcp` = ""), `NetworkTypes`, `IncludeLoopback`, `Advertised` (`Via` and
  `LAN`), `Apply(*webrtc.SettingEngine) error` (only calls SettingEngine setters, so a later setter wins), and
  `TransportOptions.PacketConns` for tests (`TransportOptions.PortMux` for the slice 5 test).
- The wiring (04 §6.6): builds the SFU after the Transport, registers 01's `sfuplane` as `Deps.Events`, calls
  `SetLimits` on settings changes, `/readyz` uses `SFU.Ready()`, a Prometheus adapter over `Metrics()`, month-to-date
  transfer from 04's `netx.TransferCounter` (not from the SFU), and shutdown order: hub first, then `SFU.Close()`,
  then `Transport.Close()`.
- `POST /api/v1/conntest` (04 §7.7) on top of `SFU.Probe`.
- The admin dashboard `GET /api/v1/admin/dashboard` with `server.process` (CPU seconds, RSS, NumCPU) plus the live
  rooms built from `Snapshot()` (the load test polls it every 5 s).

**05-web-client.md**
- The sharer sends simulcast with rids `f` and `q` using `ShareParams.encodings`, lists its m-sections in `tracks`,
  orders `setCodecPreferences` by `ShareParams.codec` / `quality.hint.codec` (High first when allowed, S4 finding 4)
  and re-offers on change, and applies `quality.hint` encodings (`active`, `maxBitrate`).
- The viewer maps tracks by `tracks`; adds `stereo=1` to its sub answer; sends decode caps and polls them while
  H.264 is missing (no client codec retries); sends `pc.restart{mode: ice}` after 3 s of ICE `disconnected` and
  `pc.restart{mode: rebuild}` on `failed`; treats an incoming sub offer with a new `ice-ufrag` as the ICE restart
  (cancels its 3 s timer and starts the 15 s rebuild timer from that offer, 05 §9); shows the four wire status
  reasons.
- The connection-test UI calls 04's endpoint. The Chrome-only e2e (05 §19.3) covers the compatible Chrome cells of
  §8.2; it measures no A/V offset (the 45 ms target is integration 5 plus the exit survey, §9.6). The manual matrix
  (05 §19.4) holds M-FF-1 (Firefox) and the throttled-downlink row (drop to `low`, back on `high` within about
  2 minutes of lifting the throttle, §17).
- 05's e2e server fixture (`startServer`, 05 §19.3), which `web/e2e/decodable.spec.ts` (README S89) uses to run
  `isshoni-loadtest` against a Chrome viewer (§17).

**06-deploy-and-ci.md**
- sysctls `net.core.rmem_max` and `wmem_max` = 8388608 in install.sh and the Docker docs; compose publishes
  `7882/udp`, `7882/tcp` and `443` with the same host and container ports.
- CI runs `go test -race ./...` (the 10×10 test outside `-short`), which includes the load-test `smoke` scenario as a
  Go test in `test-go` against an in-process server (S89); release builds
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
| 5 | Transport integration | `api.go`: pub/sub/probe `SettingEngine`s on 04's `netx.Transport`, remote-candidate filter (needs 04's netx slice) | `api_test`: candidates (UDP 7882, TCP 443 via a `netx.ListenPortMux` on 127.0.0.1:0 passed as `TransportOptions.PortMux`, TCP 7882), no trickle, filter table |
| 6 | Core model, one layer | SFU, Room, Participant, Conn actor, pub/sub PCs with `gen`/`neg` and `tracks`, Share (media states), Layer, DownTrack (single layer, no RTX), `Signaler`, `RoomEvents`, `sfutest.Harness`, DTLS gate, PLI on connect, `Close` | Integration 1 passes; goleak clean; `Close` leaves no goroutines |
| 7 | Simulcast and selection | rids, `UpdateSubscriptions`, interest mask, keyframe switching, PLI throttle, audio follows focus, transceiver reuse, `SubscriptionStateEvent`, `ShareParams` per preset (§8.6) | Integration 2, 3, 10, 17; `throttle_test` |
| 8 | Repair and sync | NACK/RTX from cache, duplicate drop, SR forwarding and translation, abs-send-time, padding | Integration 4, 5, 6, 19; `rtx_test` |
| 9 | Resilience and guards | media states (`stalled`), pub rebuild by `gen`, sub rebuild (including from `closed`), ICE restart, `Resync`, sub offer re-sends, stale offers, handshake timeout, PC grace, `ClosePC`, all §12 guards | Integration 7, 8, 9, 11, 14, 15 |
| 10 | Codec policy and Firefox | room policy with hysteresis, answer filtering (verify Pion honours preferences, else fallback), Opus preset edit, caps-driven video, sub rebuild, `Bind` guard with 20 s × 9 retries | Integration 12, 13; `policy_test`, `sdpcheck_test` |
| 11 | Downlink adaptation | allocator, pacer, `sfutest/rembsim` (`server_limit` is later) | `allocator_test`, `rembsim` test, `pacer_test`, integration 16 |
| 12 | Observability | `Snapshot`, `ConnStats`, `Metrics`, logs | a test reads every metric after integration 2 with non-zero counters where expected; no SDP or IPs in info logs (log capture test) |
| 13 | Uplink control | `UplinkPolicy`, admin cap REMB and hint, `SetLimits`, layer pausing (stretch; cuttable to M5 without a protocol change) | a test with `MaxShareKbps` → REMB on the pub PC every 1 s; layer pausing hint after 10 s unwatched and resume with PLI |
| 14 | Connection-test probe | `probe.go`, probe APIs, echo channel | Integration 18; limits enforced |
| 15 | Decodable fake and load test | decodable H.264 and the Opus asset; `cmd/isshoni-loadtest` (setup through 03's REST, scenarios, measurements, JSON, exit codes) on 01's `internal/client/signal`; `web/e2e/decodable.spec.ts` | `fake` tests; `smoke` passes as a Go test against an in-process server (S89); `decodable.spec` (S89, not a 05 slice) runs `isshoni-loadtest -scenario smoke -decodable -publishers 1 -subscribers 0` against 05's e2e server fixture, and a Chrome viewer shows `framesDecoded > 0` |
| 16 | Load gate | run `focus` on a 4 vCPU VPS from a second VM; fix what fails | every §15.2 criterion passes; `all-high` knee recorded in the release notes draft |

**What README S29 built** (the first part of slice 6: the model, the Conn actor and both PCs, without a media
path). It declares the whole §6 and §13 API. From slice 7 it brought forward `StartShare` and `UpdateShare` with the
§8.6 numbers, and the part of `UpdateSubscriptions` that the sub PC's offers need (subscriptions with their two
DownTracks, the 256 and 64 guards); layer selection, `SubscriptionStateEvent` and transceiver reuse stay in slice 7
(§9.3). The methods of slices 9, 10 and 14 return the not-implemented error of §6.3, and shares stay `pending`
until the media path of README S41 sees a keyframe.

**What README S41 built** (the rest of slice 6: `Layer`, `DownTrack`, the ticker; integration 1). §9.3 and §9.7 say
how: the DTLS-ready gate follows the sub PC's state, a stream that Pion took without sending restarts on a
keyframe, a pending share asks for its own first keyframe, and `Share.notify` is the one lock held across a
`RoomEvents` call (§5.4). From slice 8 it brought forward the duplicate and too-late drop, padding markers,
abs-send-time and the per-layer sender report; NACK/RTX and SR forwarding stay in slice 8 (§9.3).

**What README S52 built** (slice 7; integration 2, 3, 10 and 17). Layer selection is §10.1 with what it settled:
the choice is made again whenever a layer of the share comes or goes, under `Share.mu` and then `DownTrack.mu`
(§5.4), and a forwarded layer that ends is over at once. `SubscriptionStateEvent` is §6.2: sent only on a change,
at once for what the client asked for, and at most once per 250 ms and subscription for what the media path did,
with `no_layer` also for a request for `high` that gets the preview layer. Transceiver reuse is §9.3: the sub PC's
own list of spares decides when Pion's `AddTrack` may take one. The server's cap on a subscription is in place
for slices 10 and 11. The tests are in §17 (`throttle_test.go`, `subscription_test.go`, `simulcast_test.go`,
`sfutest.WithoutCutFrames`).

**What README S57 built** (slice 9; integration 7, 8, 9, 11, 14 and 15, in `recovery_test.go`, with the unit
tests of the timers and guards in `guards_test.go`).
- **The `stalled` state** (§5.3): a live share is stalled when its pub PC has not been connected for 2 s (the
  ticker), at once when that PC failed, and at once when its last video track ended (the PC closed, or a new
  `gen` replaced it). It is live again with the first keyframe that arrives while the pub PC is connected;
  `LiveAt` stays the first time. Each way in and out goes by what the publishing Conn's actor has seen of its pub
  PC, so they can't flap against each other.
- **The sub PC's whole negotiation table** (§5.3): the 15 s re-send of an unanswered offer, ICE restarts one at a
  time, `ResetPC`, and the rebuild from `closed` on `ResetPC`, `RestartICE`, `Resync()` or a subscription change.
  `RestartICE`, `ResetPC`, `ClosePC` and `Resync()` are implemented, so a client's `pc.restart` and `pc.close` no
  longer answer `internal` (§6.3). A `ResetPC` on a Conn without subscriptions closes the sub PC and builds none;
  the next subscription builds the successor.
- **PC states**: the 10 s handshake timeout from the PC's first answer and the 30 s grace after `failed`, each
  with its `PCStateEvent` reason (§5.3); Pion's close on the client's `close_notify` stays (§5.3). A PC's
  connection state is read on the actor when a callback's post is handled, not carried by the callback, because
  Pion runs each callback in a goroutine of its own and two changes can arrive in either order; each state is
  reported once.
- **The guards of §12**: `sfu.pc_limit` and `sfu.pc_rate_limited` with what they count and who returns them
  (§6.1 "PC limits", §12), and the cap of 64 remote candidates counted as Pion keeps candidates, across trickle
  and remote descriptions, with the pub PC's candidates its own (§7.3).
- **`Unbind` ends only the binding of the sender that stopped**, so a late stop of an old sub PC's sender can't
  unbind a DownTrack that a rebuilt sub PC has bound already (§5.4).
- A pub offer with new ICE credentials in the current `gen` restarts ICE on the same PC (Pion does it and
  answers with new credentials of its own): the pub row of §6.5, `TestPubICERestart`.
- **Test tools**: `sfutest.Viewer.Rebuild`, a `DirectSignaler` that answers like a client (a new `gen` on a new
  PeerConnection, a repeated offer with the answer it gave) and can be down (`SetDown`), `PublishICERestart`, and
  `publish.Publisher.OfferICERestart`. A `FaultConn` under a Go client's own UDP mux takes that client's network
  away.
- Besides the six integration tests: `TestPCRateLimit`, `TestPCGrace`, `TestPubICERestart`, `TestClosePub`,
  `TestSubPCResetWithoutSubscriptions`, `TestShareStalled`, `TestSubOfferResend`, `TestSubICERestartRules`,
  `TestRemoteCandidateCapIsPions`, `TestPubCandidatesOfALaterGen`, and `TestCandidateKey` with `FuzzCandidateKey`,
  which tie the count's key to `ice.Candidate.Equal`.

Still to come in the paths S57 touched, when S57 merged: the last `QualityHintEvent` per share in `Resync()` (README
S88 built it, rebuilt from the current state, §6.2), and the SFU's own codec rebuilds, which the creation limit won't
count (README S69 built them, §8.5).

**What README S63 built** (slice 8; integration 4, 5, 6 and 19 in `repair_test.go`, the unit tests in
`rtx_test.go`). Repair is §9.5 in the order of its steps, with the cap of 256 sequence numbers per compound
packet, the 5 s life of a NACK record and the counters; the DownTrack's second queue, its RTCP reader for NACKs
and the RTT from the candidate pair are §9.3. Sync is §9.6, with the `lastTS + 1` rule of §9.4 for a switch to a
keyframe whose capture time was forwarded already. The publish side got the NACK filter and the cap of 10 requests
per packet, with one `webrtc.API` per pub PC (§8.1). The test tools are §15.1 (`RedundantRTX`, `Padding`, the
Viewer's own NACKs, `HarnessOptions.FaultyUDP`) and §17 (`InSeqOrder`).

**What README S69 built** (slice 10; integration 12 and 13 in `firefox_test.go`, `policy_test.go`, and the codec
filter's tests in `sdpcheck_test.go`).
- **The room codec policy** of §8.3 in `room.go`: high while every viewer (a Conn that subscribes, with H.264 in
  its caps) can receive High, cb at once otherwise, and high again only after 60 s without a viewer that needs cb.
  It is worked out again where the viewer set changes (`Join`, a Conn's close, `SetDecodeCaps`) and by the ticker
  once a second for the way back. A change goes to the hub (`CodecPolicyChanged`) and, as a `CodecPolicyEvent`, to
  the Conn that publishes each share, in the order of the changes under `Room.policyMu` (§5.4); new shares get it
  in `ShareParams.Profile`, and `Resync()` sends it again per own share (§6.5).
- **The pub answer's codec filter** (§8.4 step 3, `Conn.filterCodecs`): set on every offer, chosen from the codecs
  of the offer that made the transceiver (`pubPC.offered`) in the current offer's order, and an offer without an
  allowed profile answered with what it has. Pion honours the transceiver's preferences, so the design's fallback
  wasn't needed.
- **Viewers without H.264** (§8.5, `codecstate.go`): `SetDecodeCaps`, the caps-driven video, the codec rebuilds
  with their rules (equal caps a no-op, a rebuild also when the codec holds a subscription, a stale empty sub PC
  rebuilt by the next subscription, a closed sub PC never rebuilt for the codec), and the guard driven by what
  `Bind` could bind, with its 20 s × 9 retries and `decoder_failed`. None of these rebuilds counts toward the PC
  creation limit (§12). `SetDecodeCaps` is implemented, so a client's `caps.update` no longer answers `internal`
  (§6.3); `Probe` is the one method left, until README S76.
- **The decoders' cap** is `Subscription.codec`, apart from the allocator's cap, and `codec_mismatch` is a derived
  reason (§10.1).
- **Test tools**: `publish.Options.Profiles`, `Publisher.SetProfile` and `publish.ProfileSource`;
  `sfutest.ProfileSource`; `sfutest.ViewerOptions.H264`, `NoH264` and `Viewer.SetH264`; the codec clock and
  `Timings.CodecRetry` in `export_test.go` (§15.1, §17).

Still to come in the paths S69 touched: the codec downgrade counters of §13 (`isshoni_sfu_downgrades_total` by
reason, README S79). README S79 built them since: integrations 12 and 13 read them from `Metrics`.

**What README S79 built** (slice 12; `stats.go`, `counters.go`, `pionlog.go`, with `observability_test.go` and
`stats_test.go`, and `Metrics` checks added to `repair_test.go` and `firefox_test.go`). `Conn.Stats`, `SFU.Snapshot`
and `SFU.Metrics` report everything of §13 but what slice 11 adds (the downlink estimate and the `bandwidth`
downgrades) and `server_limit`, which is later. §13 "What README S79 settled" has the rules it chose: totals per SFU
that Layers and DownTracks count into, so counters never go back; transport and RTT only while a PC is connected, the
pub PC's RTT read too; every label in `Snapshot`'s totals; what counts as a layer switch and as a downgrade; the log
levels, with Pion's info lines and its lines that hold an address at debug. It also reads the viewer's receiver
reports for `DownTrackStats.LossPct` (§9.3) and gives each DownTrack its bitrate on the ticker. The SFU's count of
handshake timeouts moved into its totals. Its one handover outside the package, netx's Pion info lines at info when
the log level is debug, the group 11 integration fixed (04 §7.3).

**What README S84 built** (slice 11; `allocator.go` and `pacer.go`, with `allocator_test.go`, `pacer_test.go`,
`adaptation_test.go`, `sfutest/link.go` and the package `sfutest/rembsim`). One downlink allocator per subscriber
Conn (§10.2), fed by the RTCP readers and queues of its DownTracks (§9.3) and run by the Conn's actor on the
ticker's once-a-second look and at once on a fall or loss; one pacer per video DownTrack (§9.8), which
retransmissions go through first (§9.5); `ConnStats.DownlinkEstimate` and the `bandwidth` downgrades of §13.
`server_limit` stays later. Where the design left a choice it settled: loss as the link lost it, with the SFU's own
gaps out, an outage only a new start, evidence only from after a decision and a verdict only on 20 packets; a
share without a smaller layer never capped; every cap decided again when a request changes while `limited`; and
the guards on the viewer's RTCP of §12 (from its review: a flood of receiver reports had cost quadratic work under
the allocator's lock, and a REMB past an int64 broke the arithmetic on arm64). Two tests changed with it: integration
4 loses 5 % of the video instead of 10 %, and integration 16's first trial fails on REMB (§17). The `rembsim`
subpackage was outside S84's listed paths (README §5: a package means its own directory); the spec asks for it, and
group 12 accepted it.

**What README S88 built** (slice 13, cuttable and not cut: §11 "As README S88 built it"; `uplink.go`, with changes
to `share.go`, `layer.go`, `allocator.go`, `subscription.go`, `conn.go` and `sfu.go`; `publish.Publisher.SetActive`;
`sfutest.ApplyHint`; the tests in §17). The admin's cap reaches running shares as a quality hint with the reason
`admin` and as one REMB per pub PC with the sum of its shares' caps, and three REMBs of 1 Gbit/s once the cap goes;
layer pausing is on by default (README §6 "Decided after group 13") and pauses only the full layer of a share with a
preview layer, 10 s after the last request for `high` ended (counted from the share's start or the first look that
found the request gone) and resumes it at once on any new request, with a PLI that a paused layer's dropped keyframe
requests leave unthrottled; `UpdateShare`'s encodings and `Resync`'s rebuilt hint carry the cap and pause state; the
downlink allocator weighs a paused or just resumed layer at its maximum; and layers measure their interarrival
jitter per frame. Its review fixes are in the text: the allocator's weight (S88-R1), the 10 s counted from when the
request ended (S88-R2), one jitter sample per frame (S88-R3), `UpdateShare` no longer taken as told (S88-R4), the
lock claim of `uplinkStats` (S88-R5). The group 13 e2e run, with pausing on, passed every spec.

**What README S89 built** (slice 15's tool: §15.2 "As README S89 built it"; `cmd/isshoni-loadtest`,
`web/e2e/decodable.spec.ts`; the tests in §17). It adds `-v`, exit 130, `client.go`, the reported fields beyond
§15.2's shape, `smoke`'s limits, the expected egress from the measured rates, all-high's knee rule, the memory check
skipped without an RSS, and `LOADTEST_BIN` for the spec, which the group 13 integration wired into CI (06 §8.2). It
added the module `golang.org/x/term` v0.47.0 (BSD-3-Clause), which only the tool links.

## Decisions taken at integration (formerly open questions)

1. **Layer pausing** (§11): on by default, with the switch `sfu.pause_unwatched_layers`; the slice is cuttable to M5.
2. **Who may run the connection test** (§7.6): every signed-in user, one probe per user and transport (a new one
   replaces the old one); only admins see provider fix text (05).

Decided after group 10 (engineering calls the owner delegated; README §6):

3. **The sub PCs keep checking candidate pairs for the length of the failed timeout**, so a sub ICE restart that a
   viewer asks for while its network is still gone connects once the network is back (§7.3; 01 §10.4, §11.5 B).
   With Pion's default the SFU, the controlling agent of a sub PC, gave up on each pair after 7 unanswered checks,
   about 1.5 s, and such a viewer came back only through the 15 s rebuild. README F10 builds it, together with
   keyframe requests that wait while the share's pub PC is away, so that the request of the pub PC's own reconnect
   is not swallowed by the 500 ms throttle (§9.7). The setting is in the shared `SettingEngine`, so the pub PCs get
   it too; the probe APIs keep Pion's 7 (§7.3).
4. **The M1 security review's two findings in this doc** (§12): R3, peer-reflexive sources, is bounded below Pion
   in 04's netx, 64 per local ICE ufrag (README F06); R1, pub-PC tracks under SSRC churn, has a fix that waits for
   the owner (README F07).

Decided after group 11 (an engineering call the owner delegated; README §6 (2)):

5. **Pion's info lines are debug lines**, and so are its warnings and errors that hold an IP address (§13,
   `pionlog.go`; README S79). Pion logs about ten info lines for each PeerConnection that connects and closes,
   which repeat without ids what the SFU logs itself, and its lines about stray checks name the client's address.
   04's netx does the same for its muxes' Pion logger, whose warnings were debug lines already (04 §7.3). So at
   info and above no line of Pion's names an address, and the log capture test checks every component (§13).

Decided after group 12 (engineering calls the owner delegated; README §6 (1), (2), (3)):

6. **Retransmissions are paced, ahead of new media** (§9.5, §9.8; README S84, the question S63 left open). A video
   retransmission goes through the DownTrack's token bucket like new media, and the writer serves the RTX queue
   first, so a repair never waits behind a frame and a large frame asked for again leaves spread like the frame
   itself. Audio and its retransmissions are never paced.
7. **The downlink allocator reads loss as the link lost it** (§9.3, §10.2, §12). The own sequence numbers the SFU
   never sent are taken out; a receiver report more than 2 s after the last is an outage and only starts the count
   again; a report is evidence about a decision only when all of its interval came after it; a verdict needs 20
   packets; and a viewer's reports count at most once per 200 ms each, at most 1024 kept. Since receivers count
   packets that RTX brought back as lost, integration 4 loses 5 % of the video, under the 10 % that is congestion
   (§17).
8. **Two rules of the allocation, and what integration 16 shows** (§10.2, §17). A share without a smaller layer
   is never capped, since a cap would leave its viewer without video; while `limited`, any change to a request
   decides every cap again, newest `high` first, which can also give an older request its full layer back. With
   `rembsim`, the first failed trial of integration 16 fails on a falling REMB rather than on loss; either fails a
   trial, and the test accepts both.

Decided after group 13 (engineering calls the owner delegated; README §6 "Decided after group 13"):

9. **Layer pausing is on by default** (§11; README S88, which was cuttable and was not cut). With it on, the group 13
   integration ran every Go test and the whole e2e suite green, `isshoni-loadtest`'s smoke run and S90's tests
   included. `sfu.pause_unwatched_layers = false` turns it off.
10. **The admin's cap is one REMB per pub PC** (§11): the sum of the caps of the shares the PC carries, naming all
    their video SSRCs, and none while one of them has no cap, because Chrome applies a REMB's bitrate to everything
    the PC sends whichever SSRCs it names. When the cap goes, three REMBs of 1 Gbit/s lift the limit a browser keeps
    from the last REMB; never a REMB of 0.
11. **What pausing counts and when** (§11, §9.7, §10.2): any request for `high`, also from a viewer whose decoders
    can't take the video; the 10 s from the share's start or from the first look that found the last request gone,
    so a layer is never paused before 10 s without one; never the only layer of a share without a preview layer.
    A paused layer's keyframe requests do nothing, and the downlink allocator weighs a paused or just resumed layer
    at its maximum. `UpdateShare`'s encodings and `Resync`'s hint carry the current cap and pause state.
12. **`isshoni-loadtest` departs from §15.2 in five places** (README S89): exit 130 for an interrupt (the accounts
    are still deleted); media that never came is a failure (1), not a setup error; `-duration` includes the
    warm-up; the expected egress comes from the publishers' measured rates; and the memory check is skipped when
    the server reports no RSS. `smoke`'s limits and all-high's knee are §15.2's "As built".
