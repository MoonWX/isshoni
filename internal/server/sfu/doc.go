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
//	Layer RTP reader  1 per incoming   reads the track (pubTrack.readRTP) until it ends; for the Layer the track is
//	                  track            attached to: parse, one copy, cache insert, fan-out to the DownTracks that
//	                                   want the layer (Layer.handleRTP)
//	Layer RTCP reader 1 per incoming   reads the track's RTCP, per rid for simulcast, which also runs the publish
//	                  track            side's interceptors; keeps the layer's latest sender report
//	DownTrack writer  1 per DownTrack  takes packets off the DownTrack's queue: gate, munge, fresh header, write
//	                                   (DownTrack.run); owned by the subscriber's Conn
//	DownTrack RTCP    1 per DownTrack  the viewer's PLI and FIR for the track, until its sender stops
//	                                   (DownTrack.readRTCP); owned by the sub PC
//	Ticker            1 per SFU        every 250 ms, and at once for a state change: the ShareUpdated calls that are
//	                                   due; every second: each layer's rates and cache size. Started by the first
//	                                   Join, stopped by Close. README S57, S69, S84 and S88 add to it
//	Probe             1 per probe PC   README S76
//
// Rules:
//   - Lock order: SFU.mu → Room.mu → Share.mu → DownTrack.mu. Each is held briefly and never across a Pion call, a
//     Signaler or RoomEvents call, or a channel send that can block.
//   - Share.notify is outside that order: it only puts a share's RoomEvents calls in order (every ShareUpdated before
//     the ShareEnded, and nothing after it). The ticker and SFU.endShare hold it across those calls, which never
//     block, take Share.mu inside it, and hold no other lock when they take it.
//   - A Conn's PCs, subscriptions, negotiation state and candidate buffers belong to its actor and have no lock.
//   - The media path takes no lock per packet but the cache's and, in the writer, the DownTrack's own (uncontended).
//     A Layer's read loop reads the share's fan-out list and each DownTrack's interest mask as atomics, and hands
//     packets over with a channel send that never blocks: a full queue drops and counts.
//   - WriteRTP and WriteRTCP are media calls, not signaling calls: the media goroutines make them, and so may any
//     actor, also for another Conn's pub PC (a keyframe request). Pion allows them from any goroutine and they
//     never block. Keyframe requests are throttled per layer by a compare-and-swap, so callers need no lock.
//   - Bind and Unbind of a DownTrack are called by Pion with the sender's lock held. They read the negotiation and
//     store the result, taking DownTrack.mu for a moment; that lock is never held across a Pion call, so the two
//     are only ever taken in this order.
//   - Pion callbacks (OnTrack, OnConnectionStateChange) only append to the Conn's internal event queue. They never
//     block, so a Pion call made by the actor can't deadlock on its own callback. Pion runs each callback in a
//     goroutine of its own, so their order means nothing: a state callback carries no state, and the actor reads
//     the PC's state when it handles the post (onPCState).
//   - A pubTrack reader whose RTP read fails posts the track's end to the actor, which detaches the track and takes
//     it out of pubPC.tracks (pubTrackEnded). The table holds only tracks that are still read, so an offer never
//     binds an ended track to a share.
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
//     StopShare, CloseRoom, SetLimits; StartShare, UpdateShare and the encodings per preset (02 §8.6); the ticker;
//     a share's media state (pending until its first keyframe, then live) and its ShareUpdated calls: a state
//     change at once, a layer, profile or audio change while live at most every 250 ms (02 §5.3, §6.2);
//   - conn.go: the Conn actor with its two queues, Close and Done, PC state events, the remote-candidate buffer;
//   - pubpc.go: the publish PC: HandleOffer (gen and neg, validation, the answer with its Opus and a=inactive edits,
//     the stored answer for a repeated neg), incoming tracks bound to shares by the tracks binding until they end,
//     their read loops, and the stuck state of a PC that Pion left holding an offer it refused (it takes no other
//     offer of its gen);
//   - subpc.go, subscription.go: the subscribe PC's offers with gen, neg and tracks, the 50 ms debounce,
//     HandleAnswer, the DTLS-ready gate, and the closed state: a sub PC the client closed, or one the SFU closed
//     after a fatal error (an offer it couldn't make, an answer Pion refused); UpdateSubscriptions;
//   - layer.go, downtrack.go, munger.go, packetcache.go: the media path (02 §9). A Layer is one incoming stream of a
//     share: its packets are copied once, kept in the per-Layer ring of recent packets (02 §9.2) and queued to the
//     DownTracks that want the layer; it measures its rates and asks its publisher for keyframes, at most one PLI
//     per 500 ms (02 §9.7). A DownTrack is one viewer's video or audio of a share (02 §9.3): its writer forwards
//     nothing before the viewer negotiated the track and its sub PC is connected, and nothing once that PC has
//     failed or closed (S4 finding 1: Pion drops RTP written then, silently); it starts on a packet that carries an
//     SPS, again after every such interruption, and gives every packet its place in the viewer's stream (the
//     munger, 02 §9.4) under a fresh header. The viewer gets a keyframe requested for it when its sub PC connects,
//     when a DownTrack is bound on a connected PC, when its target layer changes, on its own PLI or FIR, and for as
//     long as packets of the layer it waits for arrive without one (only every 5 s for a viewer whose stream Pion
//     has taken twice in a row without sending any of it);
//   - events.go, errors.go, types.go, config.go, stats.go, probe.go: the whole interface of 02 §6 and §13;
//   - api.go: the publish, subscribe and connection-test probe webrtc.APIs on 04's netx.Transport, the
//     remote-candidate filter (02 §7.3, 01 §17), the selected-pair label and the complete-SDP helper: the SFU never
//     trickles (02 §7.1–7.3, §7.6);
//   - codec.go, h264.go, sdpcheck.go: profiles, the payload-type table and both MediaEngines (02 §8.1–8.2),
//     keyframe-start detection and the SPS parser (02 §9.1), pub-offer validation and the answer edits (02 §8.4–8.5).
//
// What a subscription forwards is, in this slice, what it names: high is the full layer f, low the preview layer q,
// off nothing, and audio flows while Audio is set. A layer the publisher doesn't send forwards nothing.
//
// # What later slices add
//
// The whole API is declared; a method that a later slice implements returns an error that wraps ErrNotImplemented
// (RestartICE, ResetPC, ClosePC, SetDecodeCaps, Probe), or does nothing yet (Resync). In README order:
//   - S52: the rest of 02 §10.1's layer selection (high falls back to q, the reasons no_layer and no_preview_layer),
//     SubscriptionStateEvent, the tests of switching and of audio following focus, and reusing the transceivers of
//     ended shares (until then every DownTrack gets a new sendonly transceiver, so a sub PC's SDP only grows);
//   - S57: the stalled state (a share that went live stays live until it ends), RestartICE, ResetPC, ClosePC,
//     Resync, the 15 s sub offer re-send, the rebuild of a sub PC that closed (subPC.closed, whoever closed it:
//     until then its subscriptions wait and new ones fail with sfu.internal), the 10 s handshake timer, the 30 s
//     grace after failed, sfu.pc_limit and sfu.pc_rate_limited;
//   - S63: NACK/RTX from the cache (the DownTrack's RTX queue), sender reports forwarded to the viewers (a Layer
//     already keeps its latest one, for the munger), and the tests of padding and of the publisher's redundant RTX;
//   - S69: the room codec policy with its hysteresis (CodecPolicy is ProfileHigh until then), the codec filter of
//     the pub answer, SetDecodeCaps, viewers without H.264, and the codec_mismatch state of a DownTrack whose
//     viewer has no payload type for the stream's profile (it forwards nothing until then, silently);
//   - S76: Probe. S79: the transports, RTTs and counters of Snapshot, ConnStats and Metrics (the Layers and
//     DownTracks count; nothing sums them up yet). S84: the downlink allocator (REMB, receiver reports, queue
//     drops) and the pacer. S88: the admin cap as REMB and quality hint (SetLimits already caps new ShareParams),
//     layer pausing.
package sfu
