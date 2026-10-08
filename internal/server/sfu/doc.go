// Package sfu is isshoni's selective forwarding unit on Pion (docs/m1/02-sfu.md): rooms, participants and
// connections, shares with two simulcast layers, keyframe-aligned layer switches, NACK/RTX from a shared packet
// cache, forwarded sender reports, downlink adaptation and a per-room H.264 profile policy.
//
// The SFU never imports signal or protocol: 01's sfuplane adapter is the only translator between this package and
// the wire. Signal calls the methods of *SFU and *Conn (02 §6.1); the SFU answers through each Conn's Signaler and
// the room-wide RoomEvents (02 §6.2); every error is an *Error with a stable sfu.* code (02 §6.3).
//
// # Object model (02 §5.1)
//
//	SFU ── the publish, subscribe and probe webrtc.APIs, all on 04's netx.Transport
//	 └─ Room (in memory; created by the first Join, removed with its last Conn)
//	     ├─ Participant (a user in the room; one or more Conns)
//	     │    └─ Conn (one signaling connection; role full, viewer, publisher or agent)
//	     │         ├─ pubPC (the client offers)  ──► pubTracks ──► Layers of the Conn's shares
//	     │         ├─ subPC (the SFU offers)     ◄── DownTracks, one sendonly transceiver each
//	     │         └─ subscriptions[share] = {video DownTrack, audio DownTrack}
//	     └─ Share (the participant's; media from exactly one Conn's pubPC)
//	          ├─ layers f, q and audio: Layer (+ packet cache)
//	          └─ fan-out lists: the video and the audio DownTracks of its subscribers (copy-on-write)
//
// The hub owns share ids, the share lifecycle with its timeouts, and the end reasons. The SFU reports media facts
// and never ends a share by itself; it removes one when signal says so (StopShare, a Conn's Close, CloseRoom) or at
// SFU.Close. Pub offers map m-sections to shares with 01's tracks binding, read afresh on every offer, never with
// the msid; a sending m-section that carries no share is answered a=inactive and whatever arrives on it is read and
// discarded (02 §8.4). gen and neg are 01's negotiation counters, kept here for both PC kinds (02 §5.3).
//
// # Goroutines (02 §5.4)
//
//	Conn actor        1 per Conn       every signaling operation and every Pion PC signaling call (SetRemote- and
//	                                   SetLocalDescription, CreateOffer/Answer, adding and removing tracks, Close);
//	                                   it serves the command queue (64 signal calls) and the unbounded internal
//	                                   event queue (Pion callbacks, timers, work from other Conns)
//	pubTrack readers  2 per incoming   RTP and RTCP of one track of a pub PC, until the track ends; README S41 makes
//	                  track            them the Layer's RTP and RTCP loops
//	DownTrack writer  1 per DownTrack  README S41
//	DownTrack RTCP    1 per DownTrack  README S41
//	Ticker            1 per SFU        README S41 (event debounce), then S57, S69, S84, S88
//	Probe             1 per probe PC   README S76
//
// Rules:
//   - Lock order: SFU.mu → Room.mu → Share.mu → DownTrack.mu (the last one comes with the media path). Each is held
//     briefly and never across a Pion call, a Signaler or RoomEvents call, or a channel send that can block.
//   - A Conn's PCs, subscriptions, negotiation state and candidate buffers belong to its actor and have no lock.
//   - Pion callbacks (OnTrack, OnConnectionStateChange) only append to the Conn's internal event queue. They never
//     block, so a Pion call made by the actor can't deadlock on its own callback.
//   - Work for another Conn, and timer work, is posted to that Conn's internal event queue, never to its bounded
//     command queue, so no actor ever waits on another. StopShare therefore returns once the share is removed and the
//     posts are queued, however busy the subscribers are.
//   - A signal call waits for room in the command queue for at most 5 s (and no longer than its context); after
//     that it is sfu.busy (retryable) and nothing ran. Once the actor has started a call, the caller gets its result.
//   - Fan-out lists are atomic pointers to slices that are copied on write, so the media path reads them without a
//     lock.
//   - Signaler and RoomEvents implementations must not block and must not call into the SFU synchronously.
//
// # What exists (the package is built slice by slice, docs/m1/README.md §5)
//
//   - sfu.go, room.go, participant.go, share.go: the object model; New, Close, Ready, Join, the room reads,
//     StopShare, CloseRoom, SetLimits; StartShare, UpdateShare and the encodings per preset (02 §8.6);
//   - conn.go: the Conn actor with its two queues, Close and Done, PC state events, the remote-candidate buffer;
//   - pubpc.go: the publish PC: HandleOffer (gen and neg, validation, the answer with its Opus and a=inactive edits,
//     the stored answer for a repeated neg), incoming tracks bound to shares by the tracks binding;
//   - subpc.go, subscription.go, downtrack.go: the subscribe PC's offers with gen, neg and tracks, the 50 ms
//     debounce, HandleAnswer; UpdateSubscriptions; DownTrack as a webrtc.TrackLocal with its binding (02 §9.3);
//   - events.go, errors.go, types.go, config.go, stats.go, probe.go: the whole interface of 02 §6 and §13;
//   - api.go: the publish, subscribe and connection-test probe webrtc.APIs on 04's netx.Transport, the
//     remote-candidate filter (02 §7.3, 01 §17), the selected-pair label and the complete-SDP helper: the SFU never
//     trickles (02 §7.1–7.3, §7.6);
//   - codec.go, h264.go, sdpcheck.go: profiles, the payload-type table and both MediaEngines (02 §8.1–8.2),
//     keyframe-start detection and the SPS parser (02 §9.1), pub-offer validation and the answer edits (02 §8.4–8.5);
//   - layer.go, munger.go, packetcache.go: Slot, Layer and packet, the per-DownTrack seq/ts rewrite (02 §9.4) and
//     the per-Layer ring of recent packets (02 §9.2), not wired into a media path yet.
//
// # What later slices add
//
// The whole API is declared; a method that a later slice implements returns an error that wraps ErrNotImplemented
// (RestartICE, ResetPC, ClosePC, SetDecodeCaps, Probe), or does nothing yet (Resync). In README order:
//   - S41, the media path: what the pubTrack readers do with a packet (cache, fan-out), the DownTrack writer with
//     its munger and the DTLS-ready gate, keyframe requests, a share going pending → live with ShareUpdated and its
//     250 ms debounce, ShareInfo.Viewers and Profile;
//   - S52: simulcast layer selection, SubscriptionStateEvent, audio follows focus, and reusing the transceivers of
//     ended shares (until then every DownTrack gets a new sendonly transceiver, so a sub PC's SDP only grows);
//   - S57: the stalled state, RestartICE, ResetPC, ClosePC, Resync, the 15 s sub offer re-send, the rebuild of a sub
//     PC that closed, the 10 s handshake timer, the 30 s grace after failed, sfu.pc_limit and sfu.pc_rate_limited;
//   - S63: NACK/RTX from the cache, sender-report forwarding, padding;
//   - S69: the room codec policy with its hysteresis (CodecPolicy is ProfileHigh until then), the codec filter of
//     the pub answer, SetDecodeCaps, viewers without H.264;
//   - S76: Probe. S79: the rates, transports, RTTs and counters of Snapshot, ConnStats and Metrics (they hold what
//     the object model knows until then). S84: the downlink allocator and the pacer. S88: the admin cap as REMB and
//     quality hint (SetLimits already caps new ShareParams), layer pausing.
package sfu
