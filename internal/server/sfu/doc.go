// Package sfu is isshoni's selective forwarding unit on Pion (docs/m1/02-sfu.md): rooms, participants and
// connections, shares with two simulcast layers, keyframe-aligned layer switches, NACK/RTX from a shared packet
// cache, forwarded sender reports, downlink adaptation, uplink control (the admin's cap, layer pausing, and M2's
// estimator of a native publisher's uplink), the sharer's pause, a per-room H.264 profile policy and viewers without
// H.264.
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
//	                  track            side's interceptors; keeps the layer's latest sender report and tells the
//	                                   DownTracks that want the layer of it
//	DownTrack writer  1 per DownTrack  sends the retransmissions its viewer asked for, before anything else; takes
//	                                   packets off the DownTrack's queue: gate, munge, fresh header, pace (video),
//	                                   write; sends the forwarded layer's sender reports on (DownTrack.run); owned
//	                                   by the subscriber's Conn
//	DownTrack RTCP    1 per DownTrack  the viewer's NACKs (looked up in the packet caches and queued for the
//	                                   writer), PLI and FIR for the track, and its REMBs and receiver reports for
//	                                   the Conn's downlink allocator, until its sender stops (DownTrack.readRTCP);
//	                                   owned by the sub PC
//	Ticker            1 per SFU        every 250 ms, and at once for a state change: the ShareUpdated calls that are
//	                                   due; every second: each layer's rates and cache size, each DownTrack's
//	                                   bitrate, the stalled check (a live share whose pub PC has been away for 2 s),
//	                                   each room's codec policy (its way back to high after the hold), a look of
//	                                   every Conn's downlink allocator, which its actor takes (Conn.adapt), and a
//	                                   look of every Conn that publishes at its uplink, which its actor takes too
//	                                   (Conn.uplinkTick). Started by the first Join, stopped by Close
//	Probe             1 per probe PC   the connection-test probe's life (probe.run): waits for its PC to connect,
//	                                   reports the result 1 s later, and ends the probe after 20 s, when the PC
//	                                   fails or closes, when its data channel closes, or when it is stopped (a
//	                                   newer probe of the user and transport, Close); then it closes the PC.
//	                                   Pion's callbacks only wake it; the echo runs on Pion's read loop of the
//	                                   channel and only queues the message
//
// Rules:
//   - Lock order: SFU.mu → Room.mu → Share.mu → DownTrack.mu. Each is held briefly and never across a Pion call, a
//     Signaler or RoomEvents call, or a channel send that can block.
//   - Share.notify is outside that order: it only puts a share's RoomEvents calls in order (every ShareUpdated before
//     the ShareEnded, and nothing after it). The ticker and SFU.endShare hold it across those calls, which never
//     block, take Share.mu inside it, and hold no other lock when they take it.
//   - Room.policyMu is outside that order too: it only puts a room's codec policy changes and their reports in
//     order. SFU.updateCodecPolicy holds it across CodecPolicyChanged and the posts of the CodecPolicyEvents (which
//     never block), takes Room.mu inside it, and holds no other lock when it takes it.
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
//   - A viewer's NACK is handled on its DownTrack's RTCP reader with three locks, one after the other and never
//     one inside another: the DownTrack's (which epoch sent that sequence number), the cache's of that epoch's
//     Layer (is the packet still there), and the DownTrack's NACK state (was it retransmitted just now, or too
//     often; is there budget). No other lock: the budget's rate is read from atomics (the layer's measured bitrate,
//     SFU.fullMaxBps for the admin's cap, Share.audioBps), so a NACK never waits for SFU.mu or Share.mu and the
//     signaling calls that hold them. What is to be sent goes to the writer through the RTX queue, with a send that
//     never blocks. One compound packet has at most 256 sequence numbers looked at, so a viewer can't keep those
//     locks busy at will.
//   - The NACKs a pub PC sends its publisher go through the PC's NACK filter, on the goroutine of Pion's NACK
//     generator, which holds none of its locks then. The filter reads the PC's track table, an atomic pointer that
//     the Conn's actor replaces when a track comes or goes, and takes each cache's lock for one lookup at a time.
//   - A Conn's downlink allocator has a lock of its own, a leaf: the RTCP readers of the Conn's DownTracks put what
//     the viewer's REMBs and receiver reports say under it, and the actor takes it when it decides (02 §10.2).
//     The rest of the allocator's state is the actor's, and so are its caps (Conn.capSubscription). A REMB that
//     falls, or a report that shows loss, wakes the actor with one post at most (Conn.wakeAllocator), like the
//     ticker once a second; the queues' drops and the rate forwarded to the viewer are atomics. The own sequence
//     numbers a video DownTrack's writer passed over (seqGaps) have a leaf lock too, taken by the writer and by the
//     reader, never inside the allocator's.
//   - A video DownTrack's writer waits in its pacer (02 §9.8) holding no lock; its stop and its Conn's close end the
//     wait. Audio is never paced.
//   - A share's uplink control (02 §11: its policy, the cap and pause state its sharer was told) belongs to the
//     publishing Conn's actor, like everything else the Conn publishes: the ticker, SetLimits and a viewer's request
//     for high only post to that actor. What other goroutines need of it are atomics: Share.paused (a keyframe
//     request for a paused layer does nothing) and Share.resumedAt, which the actor stores before it clears a bit
//     of Share.paused (the downlink allocators weigh a paused or just resumed layer at its maximum, allocRate). The
//     viewers' requests reach the policy as each subscriber's actor published them (Subscription.shown), and a request
//     for high has that actor post a look after it published it, so the look sees it.
//   - M2's native uplink estimator (uplink_native.go, M2 01 §10.1) is one more such policy, for the shares of desktop
//     and mobile clients. What it reads besides the policy's stats are atomics: each layer's raw loss and queueing
//     delay, which the ticker computes, and the sharer's own pause (Share.sharerPaused, Share.sharerResumedAt),
//     which only the publishing Conn's actor writes (UpdateShare). A video Layer's queueing-delay trend (delayTrend)
//     has a leaf lock of its own: the Layer's RTP loop takes it to sample the first packet of each frame, the ticker
//     once a second to compute the delay; neither holds another lock then. The ticker's seconds are counted
//     (SFU.uplinkGen, an atomic it adds to once the layers have their stats), so the estimator runs its rows once
//     per second of stats, also when the actor looks more often. While the sharer has paused the share, a keyframe
//     request for it does nothing (Layer.requestKeyframe), like one for a paused layer, and the downlink allocators
//     weigh its video layers as paused ones; its end stores Share.resumedAt before it clears the pause.
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
//     of it go by the same view and can't flap against each other. Layer.requestKeyframe goes by that view too: a
//     request made while the pub PC is away does nothing, not even start the 500 ms throttle (02 §9.7).
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
//   - rtx.go, and the rest in downtrack.go, layer.go and munger.go: repair and sync (02 §9.4–9.6). A viewer's NACK
//     is answered from the packet cache of the Layer that sent the sequence number, found through the munger's
//     epochs: on the RTX stream when the viewer negotiated one for the payload type (video), else as the packet
//     again (audio), before any new media. A packet is retransmitted at most three times and not twice within
//     max(20 ms, RTT/2), which is remembered by its sequence number for 5 s: long after the cache has let go of
//     the packet, and long before the number has its next one. All retransmissions of a DownTrack stay within a
//     quarter of its layer's bitrate or maximum (at least 200 kbit/s, with a second of that in reserve). The
//     publish side's copies of packets the cache already has (a browser's redundant RTX) are dropped where they
//     arrive. A publisher is asked for a missing packet until it is in the Layer's cache, by whichever way it came,
//     and at most ten times: Pion's NACK generator writes its NACKs through the pub PC's NACK filter (pubnack.go),
//     which takes out what the cache holds. Padding-only packets are never forwarded and leave no gap. The sender
//     reports of the layer a DownTrack forwards go on to its viewer with the publisher's NTP time and the RTP time
//     on the viewer's timeline, every one that arrives and one right after each start of an epoch; a layer switch
//     keeps that timeline the capture timeline, through both layers' reports, to within a tick. So a share's audio
//     and video reach a viewer on one clock, the publisher's capture clock;
//   - allocator.go, pacer.go: downlink adaptation (02 §9.8, §10.2). Each viewer Conn has an allocator that caps its
//     high requests at the preview layer (the reason bandwidth) when its downlink is congested: a REMB at least 3 %
//     below the last one and below the rate forwarded to it, loss of 10 % over 2 s in its receiver reports (as the
//     link lost it: the sequence numbers the SFU itself never sent are taken out, and a report after an outage of
//     the viewer's network starts the count again), or more than 50 queue drops a second. It fills a budget cheapest
//     first (audio, thumbnails, then the newest high requests), and after a backoff of 10 s, doubling up to 120 s
//     with each trial that fails, lifts the caps for a 6 s trial upgrade: REMB can't grant an upgrade, so one is
//     tried. A new high request while capped is served at the full layer only if that layer's measured rate fits
//     (its maximum while the layer is paused for its sharer, and for 2 s after it resumed: 02 §11).
//     ConnStats.DownlinkEstimate is its budget while it caps, else the viewer's latest REMB. Each video DownTrack
//     paces what it sends through a token bucket of max(2.5 × the layer's maximum, 3 × its measured rate, 2 Mbit/s)
//     holding 20 ms of that: a keyframe after a still screen leaves over tens of milliseconds, not in one burst per
//     viewer. Retransmissions go through it too, first (decided in README S84);
//   - uplink.go: publisher-side control (02 §11). Each share has an UplinkPolicy, which its publishing Conn's actor
//     asks once a second, when the admin's cap changes, and at once when a viewer asks for high, so a paused layer
//     resumes without waiting. M1's policy is the admin's cap (Limits.MaxShareKbps) for every publisher, and with
//     sfu.pause_unwatched_layers the full layer paused once no viewer has asked for high for 10 s, counted from the
//     share's start or from the first look that found the last request for high gone; a viewer held at the preview
//     layer by its downlink allocator still asks for high, and a share without a preview layer is never paused.
//     While a layer is paused, and for 2 s after it resumed, the downlink allocators weigh it at its maximum, not at
//     its measured rate, which is 0 or catching up (Share.allocRate). The sharer hears of a change of its cap or
//     pause state through a QualityHintEvent with the full encodings (the reason viewers when the pause state
//     changed, else admin), and a layer that resumes is asked for a keyframe right after the hint; UpdateShare's
//     encodings and Resync say the same. While every share of a pub PC has a cap, the PC gets a REMB of their sum
//     every second, naming their video layers, and three REMBs of no limit once the cap is gone. Layers measure
//     their interarrival jitter for the policy (RFC 3550), one sample per frame;
//   - uplink_native.go: M2's native uplink estimator (M2 01 §10.1), the policy of the shares of desktop and mobile
//     clients; every other kind keeps M1's. It embeds M1's policy and caps the share at the smaller of the admin's
//     cap and an estimate: 80 % of what arrived after loss before repair (LayerUplinkStats.RawLossPct, counted at
//     each gap), 85 % of the rate once the queueing delay (LayerUplinkStats.QueueDelay: the minimum one-way delay of
//     each frame's first packet over 500 ms, above a baseline that rises at most 0.5 ms per s) has been 40 ms or more
//     on two ticks, and 15.1 % more every 2 s once both are low, until it is none again. Its changes reach the sharer
//     as hints with the reason congestion, at least 15 % and 2 s apart; admin and pause changes go out at once. The
//     sharer's own pause (share.update{paused}, Share.sharerPaused) makes keyframe requests for the share do nothing
//     and the estimator skip its ticks; its end asks each watched video layer for a keyframe. The publish PCs keep an
//     SRTP replay window of 4096 packets (api.go), so a native publisher's plain retransmissions are accepted;
//   - events.go, errors.go, types.go, config.go, stats.go: the whole interface of 02 §6 and §13;
//   - stats.go, counters.go, pionlog.go: observability (02 §13). Conn.Stats, SFU.Snapshot and SFU.Metrics never ask
//     an actor and never wait: they read what the Layers and DownTracks measure (their rates are the ticker's, once
//     a second), what each Conn publishes of its PeerConnections (Conn.views: the state and the selected pair are
//     Pion's atomics, labeled by 02 §7.1; the round-trip time is what the PC's RTCP readers read from the pair's
//     statistics every 5 s) and of its subscriptions (Subscription.shown). The counters of Metrics are totals per
//     SFU, which every Layer and DownTrack counts into as it counts for itself (a tally), so they never go back when
//     a share ends; the gauges are read from the object model when Metrics is called. A layer switch is a viewer's
//     stream going on in another layer than it was in; a downgrade is a reason the client hears that it didn't hear
//     last (Conn.tell), counted and logged at info. The SFU logs at info only ids, states and reasons: Conn join and
//     leave, PC states with the transport of the selected pair, the share lifecycle, codec policy changes and
//     downgrades; handshake timeouts and sub offers sent again are warnings. Pion's own lines come through logx's
//     bridge with two changes (pionlog.go): its info lines are debug lines, and so are its warnings and errors that
//     hold an IP address, so that nothing at info or above names a client's address, SDP or a candidate;
//   - probe.go: the connection-test probe (02 §7.6). Probe answers a browser's data-channel offer on the probe API of
//     one transport (api.go), so the answer holds only that transport's candidates, and the probe PC echoes every
//     data-channel message unchanged. A user has one probe per transport (a new one closes the old one), the server
//     20 (sfu.probe_limit, RetryAfter until the oldest one's 20 s are over), and a transport without its mux is
//     sfu.transport_disabled. The result channel gets one value: connected, with the server side of the selected
//     pair and its round-trip time 1 s later, or why the probe ended without (timeout, closed). Probe PCs are counted
//     in no Snapshot or Metrics value (02 §13). Close stops them and waits for them as it does for the Conns;
//   - pubnack.go: the NACK filter of each pub PC, which keeps Pion's NACK generator from asking a publisher for
//     packets that the Layer's cache already holds (02 §9.5);
//   - api.go: the publish, subscribe and connection-test probe webrtc.APIs on 04's netx.Transport (an API of its
//     own for each pub PC, whose NACK filter knows the PC's tracks), the remote-candidate filter (02 §7.3, 01 §17),
//     the selected-pair label and the complete-SDP helper: the SFU never trickles (02 §7.1–7.3, §7.6);
//   - codec.go, h264.go, sdpcheck.go: profiles, the payload-type table and both MediaEngines (02 §8.1–8.2),
//     keyframe-start detection and the SPS parser (02 §9.1), pub-offer validation and the answer edits (02 §8.4–8.5);
//   - the codecs (02 §8.3–8.5). room.go: the room's codec policy, high while every viewer (a Conn that subscribes,
//     with H.264 in its caps) can receive High, else cb at once, and back to high only after 60 s without a viewer
//     that needs cb; each change goes to the hub (CodecPolicyChanged) and, as a CodecPolicyEvent, to the Conn that
//     publishes each share (SFU.updateCodecPolicy), and new shares get it in their ShareParams. pubpc.go: the codec
//     filter of the pub answer, which allows only the policy's profiles, in the order of each offer
//     (Conn.filterCodecs: Pion answers with a transceiver's codec preferences). codecstate.go: SetDecodeCaps and the
//     viewers without H.264: no video m-sections while the caps list none (decoder_unavailable), a sub PC rebuilt
//     when they gain some, and the guard for caps that list H.264 while the answers refuse it (DownTrack.Bind binds
//     the refused video as unsupported, and the SFU rebuilds the sub PC every 20 s, 9 times, then decoder_failed).
//     A DownTrack whose viewer has no payload type for the stream's profile forwards nothing, and its subscription
//     says codec_mismatch, until a keyframe in a profile the viewer takes (DownTrack.mismatched).
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
//     subscription's state, a CodecPolicyEvent per share the Conn publishes, and the last quality hint of each such
//     share that got one. It ICE-restarts a sub PC that isn't connected and rebuilds one that has closed.
//   - The SFU's only rebuilds of its own are for a viewer's decoders (codecstate.go, 02 §8.5): when its caps gain
//     H.264, and the guard's retries. Neither touches a closed sub PC, which waits for the client's request all the
//     same, so its successor is always counted (below).
//
// The guards of 02 §12 that live here:
//   - One pub and one sub PC per Conn: an offer for a third is sfu.pc_limit; a second of one kind can't be asked
//     for, a new gen replaces.
//   - 10 PeerConnections of a kind that a client makes a Conn create within a minute: pub offers of a new gen, and
//     sub PCs after the Conn's first, which are ResetPC's and the successors of a closed sub PC, whichever request
//     builds them (ResetPC, RestartICE, Resync or a subscription change). One more is sfu.pc_rate_limited with
//     RetryAfter: the error of HandleOffer, ResetPC and RestartICE, and an ErrorEvent about the sub PC for Resync
//     and a subscription change, which have no error to put it in. Nothing builds the refused PC later; the
//     client asks again. The SFU's own codec rebuilds don't count.
//   - 64 remote candidates per PC and gen, trickled or in the PC's remote descriptions. A candidate is what Pion
//     keeps as one: its transport address, its type and its related address together (candidateKey), since Pion
//     pairs and checks each of those and has no cap of its own. One that comes again is counted once. The pub PC's
//     candidates are its own: those a client trickles for a later pub gen wait apart, for that gen's offer, and
//     count toward that gen's 64.
//   - 4 shares per participant; 256 subscriptions per Conn and 64 per call; the SDP guards of sdpcheck.go; the
//     command queue; the keyframe throttle; the DownTrack queues.
//   - Connection-test probes (probe.go): one per user and transport, 20 per server, 20 s each; a probe offer of at
//     most 64 KiB with one data channel and no media, whose candidates count toward 64 like a Conn PC's (a browser
//     sends none); one data channel per probe PC (a further one is closed at once); and no echo while 1 MiB of
//     echoes waits to go out, so a client that doesn't read can't make the server hold its echoes.
//
// # What later slices add
//
// The whole API of 02 §6 is implemented (README S76 did the last method, Probe), and the sentinel ErrNotImplemented
// that the unbuilt methods wrapped is gone; so are the reports of 02 §13 (README S79), downlink adaptation (README
// S84) and uplink control (README S88). What M1 still adds here is what its load gate finds (README S92). M2's
// native uplink estimator and the sharer's pause came with M2 WA28 (M2 01 §10.1, §10.3).
//
// The server's cap on a subscription (02 §10.1: effective = min(requested, cap), with its reason) is the downlink
// allocator's (Conn.capSubscription, reason bandwidth; server_limit is M5's); the viewer's decoders have a cap of
// their own (Subscription.codec).
package sfu
