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
  api.go          the webrtc.API instances: pub, sub, probe (MediaEngines, interceptors, SettingEngine on top of
                  04's netx.Transport.Apply), remote-candidate filter
  room.go         Room: participants, shares, codec policy
  participant.go  Participant
  conn.go         Conn actor: command queue, internal event queue, Resync, Close
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
internal/server/sfu/sfutest/rembsim/  REMB model copying libwebrtc's AIMD estimator (no imports of sfu or Pion;
                                      used by allocator_test and sfutest.Viewer, §17)
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
- `connected`: sub PC opens the DTLS gate and sends a keyframe request for every video DownTrack. Pub PC (again
  `connected` after a drop, an ICE restart or a rebuild): one keyframe request per video layer of its shares (§9.7).
- `disconnected`: nothing. The client drives restarts (01/05: ICE `disconnected` for 3 s → restart).
- `failed`: send `PCStateEvent`, start a 30 s PC grace. With no restart or reset by then, close the PC. For pub, its
  shares stay `stalled` until the pub PC is connected again and a keyframe arrives (Share diagram above), or the hub's
  30 s timeout ends them.
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
| Probe | 1 per probe PC | 20 s lifetime | timer |

Rules:
- Lock order: `SFU.mu` → `Room.mu` → `Share.mu` → `DownTrack.mu`. Each is held briefly and never across a Pion call,
  a `Signaler`/`RoomEvents` call or a blocking channel send.
- **One exception: `Share.notify`** (README S41). It is a mutex per share that is outside the order above and is
  the one lock held across a `RoomEvents` call. It only puts a share's reports in order: the ticker holds it while
  it makes the share's `ShareUpdated` calls, and `SFU.endShare` holds it while it ends the share, reports the state
  changes the ticker hadn't reported yet and then `ShareEnded`. So every `ShareUpdated` comes before the
  `ShareEnded`, and nothing is reported about a share after its end (§6.2). It is safe because `RoomEvents` calls
  never block (the last rule below), `Share.mu` is the only lock taken inside it, and whoever takes it holds no
  other lock.
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
// A lower gen, or a lower neg in the current gen, returns sfu.stale_offer (§5.3).
func (c *Conn) HandleOffer(ctx context.Context, pc PCKind, gen, neg uint32, sdp string, tracks []TrackBinding) (answerSDP string, err error) // PCPub only
func (c *Conn) HandleAnswer(ctx context.Context, pc PCKind, gen, neg uint32, sdp string) error // PCSub only
func (c *Conn) AddICECandidate(ctx context.Context, pc PCKind, gen uint32, cand webrtc.ICECandidateInit) error // filtered, §7.3
// For RestartICE, ResetPC and ClosePC, a gen lower than the current one for that kind is ignored (returns nil,
// logged at debug).
func (c *Conn) RestartICE(ctx context.Context, pc PCKind, gen uint32) error // PCSub only (for pub the client re-offers);
                                                                            // a closed sub PC is rebuilt instead; nil and no
                                                                            // new offer while one is under way (§5.3)
func (c *Conn) ResetPC(ctx context.Context, pc PCKind, gen uint32) error    // PCSub: close and rebuild with gen + 1 (client asked)
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
`AddICECandidate` return `sfu.bad_pc` for a `PCKind` that isn't theirs, the zero value included. `Quality`,
`Preset`, `SourceKind` and `ShareState` start at 0, where the zero value is the natural default: `QualityOff`,
`PresetAuto`, `SourceUnknown`, `SharePending`. The numbers are this package's own and may change: sfuplane maps
every enum by name (01 §15.4). Each of the seven types has a `String()` with the lower-case name (`full`, `web`,
`pub`, `high`, `auto`, `window`, `live`, …); for `Role`, `PCKind`, `Quality` and `ShareState` these are the
`Metrics` label values of §13.

**`CloseRoom`** (S29) ends every share of the room with `r` (`ShareEnded(r)` each, in `StartedAt` order) and then
closes every Conn of the room with `Close(r)`; the room is removed with its last Conn. It is the direct way for a
caller without a hub (tests, tools). The hub does not use it: a deleted room reaches the SFU through the hub's
connections, each of which ends its shares with its own reason and closes its peer (`Conn.Close`; §19 here,
01 §15.2). `Conn.Close` is idempotent, so the two ways never conflict. An unknown room is a no-op.

**`StartShare` and `UpdateShare`** exist from S29 on with the numbers of §8.6 (they are not stubs until slice 7):
- `StartShare` creates the share `pending` under the hub's id and returns `ShareParams` for its preset: `f` then `q`,
  both `Active`, `f.MaxBitrate` under the admin cap, `AudioBitrate` by preset, and `Profile` = the room's codec
  policy (`6400` for every room until slice 10 adds the policy). Errors: `sfu.role_forbidden`,
  `sfu.too_many_shares`, and the contract violations of §6.3 (an empty or reused share id, an unknown preset).
- `UpdateShare(id, ShareUpdate{Preset})` stores the new preset and returns the share's new `ShareParams`; a nil
  `Preset` changes nothing and returns the current ones. The encodings apply at once (the client sets them); the
  audio bitrate reaches the publisher with the next pub offer's answer (§8.4 step 5 reads the share's preset when it
  writes `maxaveragebitrate`). Errors: `sfu.share_not_found`, `sfu.not_owner`.
- `SetLimits` replaces the cap for every `ShareParams` returned from then on; an invalid value (a negative number)
  is logged and ignored. Telling running shares (REMB on the pub PC, a `QualityHintEvent`) is slice 13 (§11).

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
  so the re-send costs nothing.

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
	                         // probe's 20 s lifetime ends)
	msg        string        // logs only, never sent
	cause      error         // what Unwrap returns: ErrNotImplemented, or nil
}
func (e *Error) Error() string          // the code, and ": " + msg when there is one
func (e *Error) Unwrap() error          // ErrNotImplemented for a method that a later slice fills in, else nil
func (e *Error) Is(target error) bool   // errors.Is(err, &sfu.Error{Code: sfu.CodeBadSDP}) matches by Code alone

// One constant per code of the table below: CodeClosed = "sfu.closed", CodeBusy, CodeRoleForbidden, CodeBadPC,
// CodeBadSDP, CodeNoH264, CodeBadRID, CodeUnknownTrack, CodeStaleAnswer, CodeStaleOffer, CodePCLimit,
// CodePCRateLimited, CodeShareNotFound, CodeNotOwner, CodeTooManyShares, CodeTooManySubscriptions, CodeProbeLimit,
// CodeTransportDisabled, CodeInternal.

// ErrNotImplemented is wrapped by the error of an API method that a later slice of README §5 fills in.
var ErrNotImplemented = errors.New("sfu: not implemented yet")
```

Every exported method that can fail returns an `*Error` (`New` returns plain errors: a nil `Config.Transport`, a
negative limit). Callers match it with `errors.As` and switch on `Code`, or with `errors.Is` against an `*Error`
that has only `Code` set.

| Code | Returned by | Retryable | Meaning |
|---|---|---|---|
| `sfu.closed` | any Conn method | no | Conn or SFU closed |
| `sfu.busy` | any Conn method | yes | actor queue full for 5 s |
| `sfu.role_forbidden` | StartShare, HandleOffer(pub), UpdateSubscriptions | no | role can't do that |
| `sfu.bad_pc` | HandleOffer, HandleAnswer, RestartICE | no | method not valid for that PC kind (e.g. an offer for `sub`), or the PC doesn't exist. Not returned by `RestartICE`/`ResetPC` when the sub PC was closed: those calls rebuild it (§5.3) |
| `sfu.bad_sdp` | HandleOffer, HandleAnswer | no | unparsable SDP, data channel in a media PC, >8 m-lines, a pub offer >64 KiB or a sub answer >256 KiB (01 §3.3) |
| `sfu.no_h264` | HandleOffer | no | the video m-line offers no H.264 packetization-mode=1 (browser without H.264 encoder) |
| `sfu.bad_rid` | HandleOffer | no | a rid outside {f,q} (M1), or more than 2 |
| `sfu.unknown_track` | HandleOffer | no | a malformed binding: `tracks` binds one mid twice, a binding of this Conn's share has another kind than its m-section, or one share has a second video or audio m-section. Not for a sending m-section without a binding, or bound to a share that isn't a `pending`/`live`/`stalled` share of this Conn (a share that ended while its offer was in flight): §8.4 answers that m-section `a=inactive` and ignores it (01 §9 rule 4) |
| `sfu.stale_answer` | HandleAnswer | no | `gen`/`neg` don't match the outstanding offer (sfuplane drops it silently) |
| `sfu.stale_offer` | HandleOffer | no | pub offer with a `gen` lower than the current one, or a `neg` lower than the last one in the current `gen` (sfuplane sends `stale_negotiation`; the client ignores it) |
| `sfu.pc_limit` | HandleOffer | no | a third PC or a second PC of the same kind |
| `sfu.pc_rate_limited` | HandleOffer, ResetPC | yes | more than 10 client-caused PC creations per PC kind in a minute (§12) |
| `sfu.share_not_found` | StopShare, UpdateShare, UpdateSubscriptions item | no | unknown or ended share |
| `sfu.not_owner` | Conn.StopShare, UpdateShare | no | the share belongs to another Conn (the hub always calls the publishing Conn) |
| `sfu.too_many_shares` | StartShare | no | defense in depth: 4 per participant (the hub enforces 4 per user and the room soft limit first, 01 §8.7) |
| `sfu.too_many_subscriptions` | UpdateSubscriptions | no | guard: 256 per Conn, 64 items per call |
| `sfu.probe_limit` | Probe | yes | 20 probes per server already running (a user's new probe of the same transport replaces the old one, §7.6) |
| `sfu.transport_disabled` | Probe | no | the requested transport has no mux: `udp` with `listen.ice_udp` = "", `tcp443` in `tls.mode=off`, `tcp7882` with `listen.ice_tcp` = "" (04 answers 409 `transport_disabled`) |
| `sfu.internal` | any | yes; no for the two cases below | unexpected Pion error (logged with details). Also, with `Retryable` false: a call that breaks the API's contract, and a method that isn't implemented yet |

**`sfu.internal` that is not retryable** (S29). Two kinds of error use the code with `Retryable: false`, because
trying again can't help:
- **A contract violation**: the caller, not the client, got it wrong. `Join` with an empty `Room`, `Participant`,
  `User` or `Conn`, a `Role` or `ClientKind` outside its constants, a nil `Signaler`, a `Conn` id that is already
  joined, or a `Participant` that belongs to another `User`; `StartShare` with an empty share id, an id of a share
  that exists, or an unknown preset; `UpdateShare` with an unknown preset; an `UpdateSubscriptions` item with an
  unknown quality (a per-item error). These are bugs in the hub or sfuplane, so they get no code of their own: the
  log message names the call.
- **A method of a later slice** ("interfaces first", README §4): the whole §6.1 API is declared from S29 on, and a
  method whose slice hasn't landed returns an `*Error{sfu.internal}` whose `Unwrap` is `ErrNotImplemented`
  (`errors.Is(err, sfu.ErrNotImplemented)`); the message names the method and its README slice. After S29 these are
  `RestartICE`, `ResetPC` and `ClosePC` (S57), `SetDecodeCaps` (S69) and `Probe` (S76). `Resync()` has no error to
  return and only logs at debug until S57. The sentinel goes away with the last such method.

sfuplane needs no special case for either: both are `sfu.internal`, which it maps to 01's `internal` (01 §15.4).
So until its slice lands, a call that reaches one of these methods is answered `internal` on the wire: a client's
`pc.close`, `pc.restart` or `caps.update`, for example.

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
| WebSocket drops, media fine | reconnect with jittered backoff, `hello{resumeToken}` | same session → `Conn.Resync()` | re-sends an outstanding sub offer (same `gen`/`neg`), current `SubscriptionStateEvent`s, one `CodecPolicyEvent` per own share, the last `QualityHintEvent` per share (if one was sent); ICE-restarts a sub PC that isn't connected, or builds a new one if it was closed |
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

### 7.3 SettingEngine (shared by the pub and sub APIs; the probe APIs differ only in network types and muxes)

| Setting | Value | Why |
|---|---|---|
| `Transport.Apply(se)` (04) | UDP/TCP muxes, network types, interface and IP filters, rewrite rules, loopback (dev), mDNS disabled | §7.1 |
| `DisableActiveTCP(true)` | | the server never dials out |
| `SetLite(false)` | full ICE | S4 tested full ICE; no reason to change |
| Remote-candidate filter (in `AddICECandidate`, and on the candidates inside every remote SDP: pub offers, sub answers and probe offers, before `SetRemoteDescription`) | drop loopback and the server's own advertised addresses unless `Transport.IncludeLoopback` (04's `network.include_loopback`); drop unparsable candidates, host names (mDNS or not), link-local, multicast, broadcast, unspecified (`0.0.0.0/8`, `::`) and IPv4-compatible IPv6 (`::/96`) addresses always; drop RFC 1918, CGNAT, ULA and site-local (`fec0::/10`) addresses unless any `Transport.Advertised[].LAN` is true (the server itself advertises a private address, 04's LAN/Append case). IPv4-mapped and NAT64 (`64:ff9b::/96`) addresses are judged by the IPv4 address inside. Pion's `SetRemoteIPFilter` is not used: Pion also applies it to prflx candidates, which the server must keep learning | 01 §17: a client must not make the server probe internal networks. Client addresses are learned as prflx from incoming checks anyway |
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
3. **Filter codecs** on each video transceiver: `SetCodecPreferences` keeps, in the offerer's order, only the profiles
   the room policy allows. `high` allows all five; `cb` allows `42e0` and `4200`. Plus their RTX. The browser then
   has to encode an allowed profile. (Slice 10 verifies that Pion honours transceiver preferences in answers. If it
   doesn't, the fallback is to strip the other PTs from the offer before step 2.)
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
- S29 already returns this table from `StartShare` and `UpdateShare` (`share.go`; §6.1), ahead of slice 7: `q` is
  never touched by the cap, and a cap at or above 8000 kbps changes nothing.
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
  - `PictureLossIndication`, `FullIntraRequest` → `share.requestKeyframe(current slot)`.
  - `TransportLayerNack` → §9.5.
  - `ReceiverEstimatedMaximumBitrate` → `allocator.onREMB(bitrate, now)`. The same REMB reaches several DownTracks;
    the allocator keeps the latest and compares each new value with it (§10.2), so a repeated copy changes nothing.
  - `ReceiverReport` blocks for our SSRC → `fractionLost` → DownTrack loss EWMA and `allocator.onLoss`.
- **RTT** for the sub PC comes from the nominated ICE candidate pair (`pc.GetStats()`, sampled every 5 s). RR-based RTT
  is meaningless here because the forwarded SRs carry the publisher's NTP clock (§9.6).

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
  (`codec_mismatch`) and fixing it through the room's codec policy is slice 10 (README S69).
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

  Still slice 8's: the RTX queue and NACK handling (§9.5), forwarding translated SRs to the viewers (§9.6: the
  layer keeps the report, nothing sends it on yet), and its integration tests (4, 5, 6 and 19), which include the
  ones for padding and for the publisher's redundant RTX.

`DownTrack` as built differs from the struct above in what later slices add: there is no `rtxQueue` (slice 8) and
no pacer (slice 11). It has `pc atomic.Pointer[subPC]` (set by the actor before Pion can call `Bind`, so the binding
gets its gate), `forwarding atomic.Bool` (the writer forwards a layer now: `ShareInfo.Viewers`; written only with
`DownTrack.mu` held, so a writer can't leave a paused viewer listed) and `stats` with packets, bytes, queue drops,
packets without a payload type, unsent packets, write errors, epochs started and the viewer's keyframe requests.
The RTCP reader handles PLI and FIR only; NACK is slice 8's, REMB and receiver reports slice 11's.

Since slice 7 (README S52) the request and the cap are the **Subscription's**, not the DownTrack's: `reqVideo`,
`reqAudio`, `capQ` and `capReason` belong to the subscriber's actor and need no lock. The DownTrack has one field
for them, `want` (under `DownTrack.mu`): what its subscription asks of it, the video quality under the cap, or for
audio "on" or "off". It chooses its layer for that among the layers the share has (§10.1), again whenever those
change. `sender` and `transceiver` are the actor's too: the m-section the DownTrack is on (the reuse above).

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

For each lost own seq `S` in a `TransportLayerNack` (each NACK item names up to 17 seqs):
1. `e, ok := m.lookup(S, now)`, then `U = S − e.seqOff` and `p := e.layer.cache.get(U, now)`. A miss is counted
   (`nack_missed`) and skipped.
2. Skip it if `S` was retransmitted in the last `max(20 ms, rtt/2)`, or 3 times already (ring of 1024 records keyed
   by `S`).
3. Retransmission budget: a token bucket whose rate is 25% of max(the DownTrack's forwarded bitrate, the current
   layer's `MaxBitrate` after the admin cap, §8.6; audio: `AudioBitrate`), at least 200 kbps, and whose depth is 1 s at
   that rate. For an 8 Mbps `f` that is 2 Mbps with a 250 KB depth, enough to repair the large first frame after an
   idle screen, when the forwarded rate is still low. Over budget, skip and count.
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
  counted as throttled. When the write fails (the pub PC isn't connected, or is closing), nothing is retried:
  whoever still waits asks again after 500 ms. Audio layers never ask.

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
- **The cap exists and nothing sets it yet.** `Conn.capSubscription(sub, q, reason)` stores the cap and its reason
  on the subscription, applies `min(requested, cap)` and reports at once; its callers are the codec state (slice
  10) and the allocator (slice 11). `server_limit` is later.
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
  `{bitrate, SSRCs of the video layers}` on the pub PC every 1 s. When the value changes, it sends
  `QualityHintEvent{Reason: "admin"}` whose `Encodings` is the full recomputed list, with `f.MaxBitrate` capped per
  §8.6. Chrome applies REMB as an upper bound under its own GCC. The web sharer also applies the encodings through
  `setParameters` (05).
- **M1 stretch, layer pausing** (04 config key `sfu.pause_unwatched_layers`, default on; cuttable to M5 without a
  protocol change): when no video DownTrack has requested `high` for 10 s, send `QualityHintEvent{Reason: "viewers"}`
  with the full `Encodings` and `f.Active` false. As soon as one does, send it again with `f.Active` true, and a PLI.
  "Requested" is the viewer's request before the allocator's `bandwidth` cap (§10.2), so `f` keeps flowing while a
  viewer is capped, and its trial upgrades test a real layer rather than one measured at 0 bps. Browsers apply the
  list with `setParameters` (05). This saves the sharer about 8 Mbps of upload while nobody focuses the
  share, at the cost of 0.2–0.5 s more on the first focus. `q` is never paused while anyone is subscribed. A paused
  layer stays in `ShareInfo.Layers`, and pausing never changes `room.state` (§5.3).
- Every hint carries both the current cap and the current pause state. When a single hint carries both a cap change
  and a pausing change, the pausing change wins: `Reason` is "viewers".
- **Later (M2), native publishers**: a loss and receive-rate estimator per share (Galene's estimator design). It sends
  REMB plus `QualityHintEvent{MaxBitrate}` with the plan's hysteresis (changes of at least 15%, at most every 2 s).
  Browsers keep their own GCC over TWCC.

## 12. Limits and guards (M1)

Guards limit abuse, not people. Every number here is a constant in `config.go`, except the soft limits.

| Guard | Value | On violation |
|---|---|---|
| PCs per Conn | 1 pub + 1 sub | `sfu.pc_limit` |
| PC creations | 10 per minute per Conn and PC kind, counting new-gen `HandleOffer` and `ResetPC` only; internal rebuilds (§8.5) are not counted and never refused | `sfu.pc_rate_limited` (retryable); `Error.RetryAfter` = time until the oldest counted creation leaves the 1-minute window |
| ICE + DTLS handshake | 10 s to `connected`, new PCs only (not ICE restarts); DTLS context 10 s | close PC, `PCStateEvent{failed, handshake_timeout}` |
| PC after `failed` | 30 s grace | close; pub shares stay `stalled` (the hub's 30 s timeout ends them) |
| Shares | 4 per participant (defense in depth; the hub enforces 4 per user and the room soft limit, 01) | `sfu.too_many_shares` |
| Publish offer | ≤64 KiB, ≤8 m-lines, ≤2 rids from {f,q}, no data channel | §6.3 codes |
| Remote candidates | 64 per PC and gen in total (buffered or applied); the first 64 are kept | later ones dropped, logged at debug |
| Subscriptions | 256 per Conn; 64 items per `UpdateSubscriptions` | `sfu.too_many_subscriptions` |
| Actor command queue | 64; 5 s wait | `sfu.busy` |
| Share without media | none here: the hub ends shares `starting` or `stalled` for 30 s (`media_timeout`, 01 §4.4) | – |
| Keyframe requests upstream | 1 per 500 ms per layer | throttled (counted) |
| NACK | ≤1 s old, ≤3 retransmits per seq, 25% budget | skipped (counted) |
| DownTrack queues | video 1024, audio 256, RTX 256 | drop newest (counted) |
| Probe PCs | 1 per user and transport (a new one closes the old one), 20 total, 20 s lifetime | `sfu.probe_limit` (retryable) |
| UDP buffers | 8 MiB requested by 04's netx | warning in doctor (04) |

ICE-TCP first STUN 10 s and ICE-TCP per-IP limits are 04's (netx TCPMux on 443 and 7882); a violation closes the TCP
connection, not the PC.

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
  passes in `JoinParams.Conn`. 04 fills the dashboard's `connections[].transport` and `rttMs` from it.
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
	Bitrate     int     // bps, 2 s EWMA
	LossPct     float64 // from the viewer's receiver reports
	NACKsServed uint64
	Drops       uint64 // packets dropped by the DownTrack's queues
}
```

  The types don't change when the numbers arrive. Until then `Stats`, `Snapshot` and `Metrics` hold what the
  object model knows: after S29, `Stats` lists the Conn's own shares with their attached layers (`RID` only), and
  `PCs`, `Subscriptions` and `DownlinkEstimate` are empty; `Snapshot` lists rooms, shares and Conns without rates,
  viewer counts, transports or totals; `Metrics` has every label value of the table below present, with only
  `Conns` and `Shares` counting. The media path (slice 6, README S41) and slice 12 (README S79) fill in the rest.
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
interceptor, so the SFU's feedback generator does real work. No rate control in M1. A test option, `RedundantRTX`,
negotiates RTX and resends already-delivered packets on the RTX stream with the `repaired-rtp-stream-id` extension,
like Chrome's padding and probes (§17 test 19).

`sfutest.Viewer`: a Pion subscriber (NACK generator, RR, and REMB either from the `rembsim` model driven by a scripted
link capacity (§17) or fixed values injected by the test) that records per track: seq, ts, arrival, marker contents
(layer, frame, flash), gaps, recovered-by-RTX, final loss after 1 s, keyframes, SRs. It offers the S4 checks
(`CheckContinuous`, `CheckStartsOnSPS`) plus `AVOffset()` (flash vs beep, both mapped to NTP through the last
forwarded SRs).

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

Pass criteria for `focus` (after warm-up; exit 0 = pass, 1 = fail, 2 = setup error):

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
- `sdpcheck_test`: each validation code; a sending m-section without a binding, or bound to a share that isn't this
  Conn's, carries no share and is no error (and skips the per-share checks, the kind check included); the Opus edit
  per m-line (stereo and `maxaveragebitrate`) and `a=inactive` on the m-lines without a share; the answer check for
  m-lines without H.264.
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
   of unblocking on the same SSRC; the share never leaves `live`. Resume case: the viewer's signaling drops while its
   UDP is black-holed; after 3 s, `Resync()` and then the viewer's `RestartICE` arrive: exactly one ICE-restart offer
   is sent, and media resumes within 3 s of unblocking.
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
14. `TestHandshakeTimeout`: a client that never completes ICE → PC closed at 10 s ± 1 s, `PCStateEvent`. An ICE
    restart that never completes gets no server timer and no event.
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
(S89).

## 18. Interfaces other docs rely on

- **Package `internal/server/sfu`** (§6.1–6.3): `New`, `Config` (with `Transport *netx.Transport`), `Deps`, `Limits`,
  `SFU` (`Close`, which returns within 1 s and never closes the Transport, `Ready`, `Join`, `Shares`, `Share`,
  `CodecPolicy`, `StopShare`, `CloseRoom`, `SetLimits`, `Snapshot`, `Metrics`, `Probe`), `Conn` (every method in §6.1;
  every PC method takes `gen`), `JoinParams`, `StartShareParams`, `ShareParams`, `EncodingParams`, `TrackBinding`,
  `ShareUpdate`, `SubscriptionUpdate`, `DecodeCaps`, `ProfileKey`, `Role`, `ClientKind`, `PCKind`, `Quality`,
  `Preset`, `SourceKind`, `ShareState`, `EndReason`, `SubReason`, `ShareInfo`, `LayerInfo`, `Signaler`,
  `RoomEvents`, `Event` and its types (`SubscriptionStateEvent`, `CodecPolicyEvent`, `QualityHintEvent`,
  `PCStateEvent`, `ErrorEvent`), `Error` (with `Share` and `RetryAfter`; `Unwrap` and `Is`) and the `sfu.*` codes
  (`Code…` constants), `ErrNotImplemented` (§6.3), `ConnStats` and its types (`PCStats`, `OwnShareStats`,
  `SubscriptionStats`, `DownTrackStats`),
  `Snapshot` and its types (`RoomSnapshot`, `ShareSnapshot`, `ViewerCounts`, `ConnSummary`, `SnapshotTotals`),
  `Metrics` and its label values (§13), `ProbeTransport`, `ProbeResult`. Later: `RoomLimits`, `SetRoomLimits`.
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
- **`cmd/isshoni-loadtest`** CLI and exit codes (§15.2) for the `smoke` Go test in `test-go` against an in-process
  server (S89), `web/e2e/decodable.spec.ts` (S89, §17) and the exit-test pre-flight.

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

## Decisions taken at integration (formerly open questions)

1. **Layer pausing** (§11): on by default, with the switch `sfu.pause_unwatched_layers`; the slice is cuttable to M5.
2. **Who may run the connection test** (§7.6): every signed-in user, one probe per user and transport (a new one
   replaces the old one); only admins see provider fix text (05).
