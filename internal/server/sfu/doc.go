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
//	                                   due; every second: each layer's rates and cache size, and the stalled check
//	                                   (a live share whose pub PC has been away for 2 s). Started by the first
//	                                   Join, stopped by Close. README S69, S84 and S88 add to it
//	Probe             1 per probe PC   README S76
//
// Rules:
//   - Lock order: SFU.mu → Room.mu → Share.mu → DownTrack.mu. Each is held briefly and never across a Pion call, a
//     Signaler or RoomEvents call, or a channel send that can block.
//   - Share.notify is outside that order: it only puts a share's RoomEvents calls in order (every ShareUpdated before
//     the ShareEnded, and nothing after it). The ticker and SFU.endShare hold it across those calls, which never
//     block, take Share.mu inside it, and hold no other lock when they take it.
//   - A Conn's PCs, subscriptions, negotiation state and candidate buffers belong to its actor and have no lock.
//   - Which layer a DownTrack forwards is chosen with Share.mu and then DownTrack.mu held (02 §10.1): by the
//     subscriber's actor when the request changes, and by whoever attaches or detaches a layer of the share, before
//     that returns. So the choice always fits the share's layers, and a DownTrack already wants a new layer when its
//     track's first packet is read. What follows for the client (a SubscriptionStateEvent) is the subscriber actor's:
//     it gets a notice, as it does for every change of what a DownTrack's writer forwards. A subscription has at
//     most one notice in the actor's queue, and the actor reports what the media path changed at most once per
//     250 ms and subscription, so a publisher whose stream flaps can't flood its viewers' signaling.
//   - The media path takes no lock per packet but the cache's and, in the writer, the DownTrack's own (uncontended).
//     A Layer's read loop reads the share's fan-out list and each DownTrack's interest mask as atomics, and hands
//     packets over with a channel send that never blocks: a full queue drops and counts.
//   - WriteRTP and WriteRTCP are media calls, not signaling calls: the media goroutines make them, and so may any
//     actor, also for another Conn's pub PC (a keyframe request). Pion allows them from any goroutine and they
//     never block. Keyframe requests are throttled per layer by a compare-and-swap, so callers need no lock.
//   - Bind and Unbind of a DownTrack are called by Pion with the sender's lock held. They read the negotiation and
//     store the result, taking DownTrack.mu for a moment; that lock is never held across a Pion call, so the two
//     are only ever taken in this order. Unbind ends the binding of the sender that stopped and no other: a sub PC
//     that Pion closes by itself stops its senders late, when a rebuilt sub PC may have bound the DownTrack already.
//   - Pion callbacks (OnTrack, OnConnectionStateChange) only append to the Conn's internal event queue. They never
//     block, so a Pion call made by the actor can't deadlock on its own callback. Pion runs each callback in a
//     goroutine of its own, so their order means nothing: a state callback carries no state, and the actor reads
//     the PC's state when it handles the post (onPCState), and once more when a track arrives.
//   - A Conn's timers (the handshake timeout and the grace of each PC, the re-send of a sub offer, the debounce) only
//     post to its event queue too; the actor drops the post of a timer that was stopped or set again meanwhile
//     (actorTimer). They stop when their PC closes.
//   - A share's media state is decided where its facts are: pending → live and stalled → live by the read loop of
//     the layer whose keyframe arrives, live → stalled by the ticker (2 s without a connected pub PC), by the
//     publishing Conn's actor (the pub PC failed) or by whoever detaches the share's last video layer. What they
//     know of the pub PC is what its Conn's actor has seen (Conn.pubUp, an atomic), so the ways into stalled and out
//     of it go by the same view and can't flap against each other.
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
//     a share's media state and its ShareUpdated calls: a state change at once, a layer, profile or audio change
//     while live at most every 250 ms (02 §5.3, §6.2). A share is pending until its first keyframe, then live. It
//     is stalled when its pub PC hasn't been connected for 2 s, at once when that PC failed, and at once when its
//     last video track ended (the pub PC closed, or a new gen replaced it); it is live again with the first keyframe
//     that arrives while its pub PC is connected. The SFU never ends a share: the hub's timeouts do;
//   - conn.go: the Conn actor with its two queues, Close and Done; the PC states of 02 §5.3 with their events and
//     timers (below); the remote candidates; RestartICE, ResetPC, ClosePC and Resync;
//   - pubpc.go: the publish PC: HandleOffer (gen and neg, validation, the answer with its Opus and a=inactive edits,
//     the stored answer for a repeated neg), incoming tracks bound to shares by the tracks binding until they end,
//     their read loops, and the stuck state of a PC that Pion left holding an offer it refused (it takes no other
//     offer of its gen). An offer of a higher gen replaces the PC and leaves its shares, stalled until the new
//     tracks bring a keyframe; an offer with new ICE credentials restarts ICE on the same PC;
//   - subpc.go: the subscribe PC's negotiation, the whole table of 02 §5.3: offers with gen, neg and tracks, the
//     50 ms debounce, HandleAnswer, the 15 s re-send of an offer without an answer, ICE restarts (one at a time: a
//     request while one is queued, unanswered or less than 5 s old with ICE neither connected nor failed since
//     starts none), the DTLS-ready gate, and the closed state: a sub PC that its client closed, or that the SFU
//     closed after a fatal error (an offer it couldn't make, an answer Pion refused), a missed handshake or 30 s of
//     failed. A closed sub PC is replaced by one of the next gen, with every subscription in its first offer, when
//     the Conn needs a sub PC again: on ResetPC (which does the same to a PC that isn't closed), RestartICE, Resync
//     or a subscription change. The m-sections of a share that ended go inactive and are taken over by the Conn's
//     next subscription (Pion's AddTrack) once the viewer's answer has left them inactive, so a sub PC's SDP is as
//     large as the most subscriptions its Conn had at once (02 §9.3);
//   - subscription.go: UpdateSubscriptions and layer selection (02 §10.1). A request for high gets the full layer
//     f, or the preview layer q while that is all the share has; low gets q and never f; audio flows while Audio is
//     set and the share has an audio track. The choice is made again whenever a layer of the share comes or goes,
//     before the new track's first packet is read. The client hears what it gets through SubscriptionStateEvents,
//     only when the forwarded video, the audio or the reason changes: no_layer and no_preview_layer when the
//     share's layers can't serve the request, no reason while the viewer only waits for its sub PC or a keyframe.
//     What its own request changes it hears at once; what the media path changes, at most every 250 ms, the
//     latest state each time;
//   - layer.go, downtrack.go, munger.go, packetcache.go: the media path (02 §9). A Layer is one incoming stream of a
//     share: its packets are copied once, kept in the per-Layer ring of recent packets (02 §9.2) and queued to the
//     DownTracks that want the layer; it measures its rates and asks its publisher for keyframes, at most one PLI
//     per 500 ms (02 §9.7). A DownTrack is one viewer's video or audio of a share (02 §9.3): its writer forwards
//     nothing before the viewer negotiated the track and its sub PC is connected, and nothing once that PC has
//     failed or closed (S4 finding 1: Pion drops RTP written then, silently); it starts on a packet that carries an
//     SPS, again after every such interruption, and gives every packet its place in the viewer's stream (the
//     munger, 02 §9.4) under a fresh header. The viewer gets a keyframe requested for it when its sub PC connects,
//     when a DownTrack is bound on a connected PC, when its target layer changes (also because the layer it got has
//     ended: its stream is over then, and goes on with the keyframe of the layer it falls back to), on its own PLI
//     or FIR, and for as long as packets of the layer it waits for arrive without one (only every 5 s for a viewer
//     whose stream Pion has taken twice in a row without sending any of it);
//   - events.go, errors.go, types.go, config.go, stats.go, probe.go: the whole interface of 02 §6 and §13;
//   - api.go: the publish, subscribe and connection-test probe webrtc.APIs on 04's netx.Transport, the
//     remote-candidate filter (02 §7.3, 01 §17), the selected-pair label and the complete-SDP helper: the SFU never
//     trickles (02 §7.1–7.3, §7.6);
//   - codec.go, h264.go, sdpcheck.go: profiles, the payload-type table and both MediaEngines (02 §8.1–8.2),
//     keyframe-start detection and the SPS parser (02 §9.1), pub-offer validation and the answer edits (02 §8.4–8.5).
//
// # PeerConnection states, recovery and guards (02 §5.3, §6.5, §12)
//
// The client drives the recovery of both of its PCs (01 §10.4); the SFU reports each state of a Conn's current PC
// once (PCStateEvent) and cleans up:
//   - A new PC has 10 s from its first answer to get connected. One that misses that is closed and reported failed
//     with PCReasonHandshakeTimeout, which for a pub PC has the client offer the next gen. An ICE restart has no
//     timer and no event.
//   - A failed PC is kept for 30 s, for an ICE restart or a rebuild, then closed and reported closed with
//     PCReasonGraceExpired. A pub PC's shares are stalled meanwhile and afterwards.
//   - A PC whose client closes its side is closed by Pion (the DTLS close_notify), and the SFU keeps it that way
//     (02 §5.3). The other choice, SettingEngine.DisableCloseByDTLS, leaves a PC that Pion goes on reporting as
//     connected, taking every packet written to it without an error, until ICE gives up 20 s later and the grace
//     after failed 30 s after that: the gate would stay open on a dead PC, and a share would stay live without its
//     publisher. A closed pub PC is gone, and only a higher gen brings a new one; a closed sub PC waits for the
//     Conn's next request (above). ClosePC (01's pc.close) closes either kind without a report, and finds a PC that
//     the client's close_notify closed first as it wants it.
//   - Resync, after a WebSocket resume, sends again what signal may have dropped: the outstanding sub offer, each
//     subscription's state, a CodecPolicyEvent per share the Conn publishes. It ICE-restarts a sub PC that isn't
//     connected and rebuilds one that has closed.
//
// The guards of 02 §12 that live here: one pub and one sub PC per Conn (an offer for a third is sfu.pc_limit; a
// second of one kind can't be asked for, a new gen replaces); 10 PeerConnections of a kind that a client makes a
// Conn create within a minute, pub gens and sub rebuilds alike (sfu.pc_rate_limited with RetryAfter; the SFU's own
// codec rebuilds, README S69, won't count); 64 remote candidate addresses per PC and gen, trickled or in the PC's
// remote descriptions, with an address that comes again counted once; 4 shares per participant; 256 subscriptions
// per Conn and 64 per call; the SDP guards of sdpcheck.go; the command queue; the keyframe throttle; the DownTrack
// queues.
//
// # What later slices add
//
// The whole API is declared; a method that a later slice implements returns an error that wraps ErrNotImplemented
// (SetDecodeCaps, Probe). In README order:
//   - S63: NACK/RTX from the cache (the DownTrack's RTX queue), sender reports forwarded to the viewers (a Layer
//     already keeps its latest one, for the munger), and the tests of padding and of the publisher's redundant RTX;
//   - S69: the room codec policy with its hysteresis (CodecPolicy is ProfileHigh until then), the codec filter of
//     the pub answer, SetDecodeCaps, viewers without H.264, and the codec_mismatch state of a DownTrack whose
//     viewer has no payload type for the stream's profile (it forwards nothing until then, silently);
//   - S76: Probe. S79: the transports, RTTs and counters of Snapshot, ConnStats and Metrics (the Layers and
//     DownTracks count; nothing sums them up yet). S84: the downlink allocator (REMB, receiver reports, queue
//     drops) and the pacer. S88: the admin cap as REMB and quality hint (SetLimits already caps new ShareParams),
//     layer pausing, and the last QualityHintEvent per share in Resync.
//
// The server's cap on a subscription (02 §10.1: effective = min(requested, cap), with its reason) is in place
// (Conn.capSubscription); S69 and S84 are its callers.
package sfu
