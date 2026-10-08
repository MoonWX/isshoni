// SubscriberPC against the real SignalClient (on the fake WebSocket server) and the fake RTCPeerConnection
// (05 §19.1): track mapping by `tracks`, the stereo munge, the gen/neg rules of 05 §9, candidates, the recovery
// timers, and close() (the sub PC is closed locally: no pc.close).
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { createUiStore, type UiStore } from '../app/uiStore';
import { clearLog, createLogger, logLines } from '../lib/log';
import { SignalClient } from '../protocol/signal-client';
import { FakeSignalServer, makeError } from '../protocol/testing';
import type { PCOffer, PCRestart } from '../protocol/types.gen';
import { buildSdp, FakeRTCPeerConnection, parseSdpSections } from '../test/FakeRTCPeerConnection';
import { createTestPlatform } from '../test/platform';
import { createViewer, type ViewerServices } from './services';
import { SubscriberPC } from './SubscriberPC';

interface Section {
  mid: string;
  kind: 'audio' | 'video';
  /** The share this m-section carries; none = inactive. */
  share?: string;
  /** The msid stream id, when it should differ from the share (the mapping must not use it). */
  msid?: string;
}

/** A server's sub offer: one m-section per entry, `tracks` for the ones that carry a share. */
function offer(gen: number, neg: number, sections: Section[], ufrag = 'srv1'): PCOffer {
  return {
    pc: 'sub',
    gen,
    neg,
    sdp: buildSdp({
      ufrag,
      sections: sections.map((s) => ({
        kind: s.kind,
        mid: s.mid,
        direction: s.share ? 'sendonly' : 'inactive',
        ...(s.share ? { msid: { stream: s.msid ?? s.share, track: `${s.kind.charAt(0)}-${s.share}` } } : {}),
      })),
    }),
    tracks: sections.flatMap((s) => (s.share ? [{ mid: s.mid, shareId: s.share, kind: s.kind }] : [])),
  };
}

const AV_A: Section[] = [
  { mid: '0', kind: 'video', share: 's_a' },
  { mid: '1', kind: 'audio', share: 's_a' },
];

let server: FakeSignalServer;
let signal: SignalClient;
let viewer: ViewerServices;
let ui: UiStore;
let sub: SubscriberPC;

const sent = (type: string): unknown[] => server.messages(type).map((m) => m.data);
const restarts = (): PCRestart[] => sent('pc.restart') as PCRestart[];
const lastPc = (): FakeRTCPeerConnection => {
  const pc = FakeRTCPeerConnection.last;
  if (!pc) throw new Error('no RTCPeerConnection was created');
  return pc;
};

beforeEach(async () => {
  vi.useFakeTimers();
  // No jitter: the SignalClient reconnects exactly 0.5 s after the first drop, 1 s after the next (01 §10.2).
  vi.spyOn(Math, 'random').mockReturnValue(0.5);
  clearLog();
  server = FakeSignalServer.install();
  const platform = createTestPlatform();
  signal = new SignalClient({
    url: platform.signaling().url,
    client: platform.client,
    role: 'viewer',
    caps: () => platform.capsNow(),
  });
  viewer = createViewer();
  ui = createUiStore();
  sub = new SubscriberPC({
    platform,
    signal,
    registry: viewer.registry,
    log: createLogger('sub-pc'),
    store: viewer.store,
    ui,
  });
  signal.start();
  await vi.advanceTimersByTimeAsync(0);
  expect(signal.state).toBe('ready');
});

afterEach(() => {
  sub.close();
  signal.stop();
  server.uninstall();
  vi.restoreAllMocks();
  vi.useRealTimers();
});

describe('SubscriberPC: offers and tracks', () => {
  it('makes the PC on the first sub offer, answers it and reports its state', async () => {
    expect(sub.gen).toBe(0);
    expect(sub.state).toBe('idle');
    await sub.handleOffer(offer(1, 1, AV_A));

    expect(FakeRTCPeerConnection.instances).toHaveLength(1);
    expect(lastPc().config).toEqual({ iceServers: [], bundlePolicy: 'max-bundle', rtcpMuxPolicy: 'require' });
    expect(sub.gen).toBe(1);
    expect(sent('pc.answer')).toEqual([{ pc: 'sub', gen: 1, neg: 1, sdp: lastPc().localDescription?.sdp }]);
    expect(parseSdpSections(lastPc().localDescription?.sdp ?? '').map((s) => s.direction)).toEqual([
      'recvonly',
      'recvonly',
    ]);
    expect(viewer.store.getState().media).toBe('connecting');
    lastPc().setConnectionState('connected');
    expect(sub.state).toBe('connected');
    expect(viewer.store.getState().media).toBe('connected');
  });

  it('uses the ICE servers of the welcome', async () => {
    server.welcomeDefaults = { iceServers: [{ urls: ['turn:turn.example:3478'], username: 'u', credential: 'c' }] };
    server.drop();
    await vi.advanceTimersByTimeAsync(1000);
    expect(signal.state).toBe('ready');
    await sub.handleOffer(offer(1, 1, AV_A));
    expect(lastPc().config.iceServers).toEqual([{ urls: ['turn:turn.example:3478'], username: 'u', credential: 'c' }]);
  });

  it('maps tracks to shares by the offer’s `tracks`, never by msid', async () => {
    await sub.handleOffer(
      offer(1, 1, [
        { mid: '0', kind: 'video', share: 's_a', msid: 'something-else' },
        { mid: '1', kind: 'audio', share: 's_a', msid: 'something-else' },
        { mid: '2', kind: 'video', share: 's_b' },
      ]),
    );
    const [t0, t1, t2] = lastPc().transceivers;
    expect(viewer.registry.get('s_a')).toEqual({ video: t0?.receiver.track, audio: t1?.receiver.track });
    expect(viewer.registry.get('s_b')).toEqual({ video: t2?.receiver.track });
    expect(viewer.registry.get('something-else')).toEqual({});
  });

  it('re-reads the mapping on every offer: a reused transceiver gives its track to the new share', async () => {
    await sub.handleOffer(offer(1, 1, AV_A));
    const [video, audio] = lastPc().transceivers;
    // s_a ended and s_b took over its video m-section; the audio m-section went inactive. The msid stays the same
    // here, so the browser fires no `track` event: the mapping alone moves the track.
    await sub.handleOffer(
      offer(1, 2, [
        { mid: '0', kind: 'video', share: 's_b', msid: 's_a' },
        { mid: '1', kind: 'audio' },
      ]),
    );
    expect(viewer.registry.get('s_a')).toEqual({});
    expect(viewer.registry.get('s_b')).toEqual({ video: video?.receiver.track });
    expect(viewer.registry.shareIds()).toEqual(['s_b']);

    // And back with a `track` event (the msid changed): the entry is replaced, not doubled.
    await sub.handleOffer(
      offer(1, 3, [
        { mid: '0', kind: 'video', share: 's_c' },
        { mid: '1', kind: 'audio', share: 's_c' },
      ]),
    );
    expect(viewer.registry.shareIds()).toEqual(['s_c']);
    expect(viewer.registry.get('s_c')).toEqual({ video: video?.receiver.track, audio: audio?.receiver.track });
  });

  it('logs and ignores a track on a mid that the offer does not map', async () => {
    const o = offer(1, 1, [...AV_A, { mid: '2', kind: 'video', share: 's_x' }]);
    o.tracks = o.tracks.filter((t) => t.mid !== '2');
    await sub.handleOffer(o);
    expect(viewer.registry.shareIds()).toEqual(['s_a']);
    expect(logLines().some((l) => l.level === 'warn' && l.msg.includes('without a mapping'))).toBe(true);
  });

  it('adds stereo=1;sprop-stereo=1 to the Opus fmtp of its answer', async () => {
    await sub.handleOffer(offer(1, 1, AV_A));
    const [answer] = sent('pc.answer') as { sdp: string }[];
    expect(answer?.sdp).toContain('a=fmtp:111 minptime=10;useinbandfec=1;stereo=1;sprop-stereo=1');
    expect(lastPc().localDescription?.sdp).toBe(answer?.sdp);
  });

  it('answers without the munge when the browser rejects the munged answer', async () => {
    await sub.handleOffer(offer(1, 1, AV_A));
    lastPc().failNext('setLocalDescription', new DOMException('bad fmtp', 'InvalidModificationError'));
    await sub.handleOffer(offer(1, 2, AV_A));
    const answers = sent('pc.answer') as { neg: number; sdp: string }[];
    expect(answers).toHaveLength(2);
    expect(answers[1]?.neg).toBe(2);
    expect(answers[1]?.sdp).toContain('a=rtpmap:111 opus/48000/2');
    expect(answers[1]?.sdp).not.toContain('stereo=1');
    expect(logLines().some((l) => l.level === 'warn' && l.msg.includes('stereo answer was rejected'))).toBe(true);
    expect(restarts()).toEqual([]);
  });

  it('never logs SDP or candidates', async () => {
    await sub.handleOffer(offer(1, 1, AV_A));
    lastPc().emitIceCandidate({ candidate: 'candidate:1 1 udp 2122260223 192.0.2.7 54321 typ host', sdpMid: '0' });
    const text = JSON.stringify(logLines());
    expect(text).not.toContain('v=0');
    expect(text).not.toContain('192.0.2.7');
  });
});

describe('SubscriberPC: gen and neg (05 §9)', () => {
  it('replaces the PC for a higher gen and drops everything of an older one', async () => {
    await sub.handleOffer(offer(1, 1, AV_A));
    const first = lastPc();
    const oldTrack = viewer.registry.get('s_a').video;

    await sub.handleOffer(offer(2, 1, AV_A, 'srv2'));
    const second = lastPc();
    expect(second).not.toBe(first);
    expect(first.signalingState).toBe('closed');
    expect(sub.gen).toBe(2);
    expect(viewer.registry.get('s_a').video).toBe(second.transceivers[0]?.receiver.track);
    expect(viewer.registry.get('s_a').video).not.toBe(oldTrack);

    // Stale: an offer and a candidate of gen 1 change nothing and get no answer.
    await sub.handleOffer(offer(1, 2, AV_A));
    await sub.handleIce({ pc: 'sub', gen: 1, candidate: { candidate: 'candidate:9 1 udp 1 192.0.2.9 9 typ host' } });
    expect(FakeRTCPeerConnection.instances).toHaveLength(2);
    expect(second.remoteCandidates).toEqual([]);
    expect((sent('pc.answer') as { gen: number; neg: number }[]).map((a) => [a.gen, a.neg])).toEqual([
      [1, 1],
      [2, 1],
    ]);
  });

  it('resends the stored answer for a repeated neg and ignores an older neg', async () => {
    await sub.handleOffer(offer(1, 1, AV_A));
    await sub.handleOffer(offer(1, 2, AV_A));
    const pc = lastPc();
    const remote = pc.remoteDescription?.sdp;
    const setRemote = vi.spyOn(pc, 'setRemoteDescription');

    await sub.handleOffer(offer(1, 2, [{ mid: '0', kind: 'video', share: 's_other' }]));
    const answers = sent('pc.answer') as { neg: number; sdp: string }[];
    expect(answers.map((a) => a.neg)).toEqual([1, 2, 2]);
    expect(answers[2]).toEqual(answers[1]);

    await sub.handleOffer(offer(1, 1, [{ mid: '0', kind: 'video', share: 's_other' }]));
    expect(sent('pc.answer')).toHaveLength(3);
    expect(setRemote).not.toHaveBeenCalled();
    expect(pc.remoteDescription?.sdp).toBe(remote);
    expect(viewer.registry.shareIds()).toEqual(['s_a']);
  });

  it('applies offers one after another, in the order they came', async () => {
    const all = Promise.all([
      sub.handleOffer(offer(1, 1, AV_A)),
      sub.handleOffer(offer(1, 2, [...AV_A, { mid: '2', kind: 'video', share: 's_b' }])),
      sub.handleOffer(offer(1, 3, [...AV_A, { mid: '2', kind: 'video' }])),
    ]);
    await all;
    expect((sent('pc.answer') as { neg: number }[]).map((a) => a.neg)).toEqual([1, 2, 3]);
    expect(lastPc().signalingState).toBe('stable');
    expect(viewer.registry.shareIds()).toEqual(['s_a']);
  });

  it('ignores offers for the pub PC', async () => {
    await sub.handleOffer({ ...offer(1, 1, AV_A), pc: 'pub' });
    expect(FakeRTCPeerConnection.instances).toHaveLength(0);
    expect(sub.gen).toBe(0);
  });
});

describe('SubscriberPC: candidates', () => {
  const host = (n: number): string => `candidate:${String(n)} 1 udp 2122260223 192.0.2.${String(n)} 50000 typ host`;

  it('trickles local candidates with pc.ice and marks the end', async () => {
    await sub.handleOffer(offer(1, 1, AV_A));
    lastPc().emitIceCandidate({ candidate: host(1), sdpMid: '0', sdpMLineIndex: 0, usernameFragment: 'abcd' });
    lastPc().emitIceCandidate({ candidate: host(2), sdpMid: null, sdpMLineIndex: null });
    lastPc().emitIceCandidate(null);
    expect(sent('pc.ice')).toEqual([
      { pc: 'sub', gen: 1, candidate: { candidate: host(1), sdpMid: '0', sdpMLineIndex: 0, usernameFragment: 'abcd' } },
      { pc: 'sub', gen: 1, candidate: { candidate: host(2) } },
      { pc: 'sub', gen: 1 },
    ]);
  });

  it('does not send a candidate longer than the protocol allows', async () => {
    await sub.handleOffer(offer(1, 1, AV_A));
    lastPc().emitIceCandidate({ candidate: `candidate:${'x'.repeat(600)}`, sdpMid: '0' });
    expect(sent('pc.ice')).toEqual([]);
  });

  it('buffers remote candidates until the remote description is set: at most 64, the oldest dropped', async () => {
    for (let n = 1; n <= 70; n++) {
      await sub.handleIce({ pc: 'sub', gen: 1, candidate: { candidate: host(n), sdpMid: '0' } });
    }
    expect(FakeRTCPeerConnection.instances).toHaveLength(0);
    await sub.handleOffer(offer(1, 1, AV_A));
    const pc = lastPc();
    expect(pc.remoteCandidates).toHaveLength(64);
    expect(pc.remoteCandidates[0]).toEqual({ candidate: host(7), sdpMid: '0' });
    expect(pc.remoteCandidates.at(-1)).toEqual({ candidate: host(70), sdpMid: '0' });

    // With a remote description they are added at once; one without mid or index goes to the first m-section.
    await sub.handleIce({ pc: 'sub', gen: 1, candidate: { candidate: host(71) } });
    expect(pc.remoteCandidates.at(-1)).toEqual({ candidate: host(71), sdpMLineIndex: 0 });
    // The end-of-candidates marker adds nothing.
    await sub.handleIce({ pc: 'sub', gen: 1 });
    expect(pc.remoteCandidates).toHaveLength(65);
  });

  it('keeps candidates for the gen whose offer is still to come', async () => {
    await sub.handleOffer(offer(1, 1, AV_A));
    await sub.handleIce({ pc: 'sub', gen: 2, candidate: { candidate: host(1), sdpMid: '0' } });
    expect(lastPc().remoteCandidates).toEqual([]);
    await sub.handleOffer(offer(2, 1, AV_A, 'srv2'));
    expect(lastPc().remoteCandidates).toEqual([{ candidate: host(1), sdpMid: '0' }]);
  });

  it('sends the candidates and the answer that signaling could not take once it is ready again', async () => {
    await sub.handleOffer(offer(1, 1, AV_A));
    lastPc().setConnectionState('connected');
    server.autoOpen = false;
    server.drop();
    expect(signal.state).toBe('backoff');
    lastPc().emitIceCandidate({ candidate: host(1), sdpMid: '0' });
    // The server's next offer can't arrive without the socket; a caller may still hand one over.
    await sub.handleOffer(offer(1, 2, AV_A));
    expect(sent('pc.ice')).toEqual([]);
    expect(sent('pc.answer')).toHaveLength(1);

    await vi.advanceTimersByTimeAsync(1000);
    server.accept();
    await vi.advanceTimersByTimeAsync(0);
    expect(signal.state).toBe('ready');
    expect(server.welcomes.at(-1)?.resumed).toBe(true);
    expect(sent('pc.ice')).toEqual([{ pc: 'sub', gen: 1, candidate: { candidate: host(1), sdpMid: '0' } }]);
    expect((sent('pc.answer') as { neg: number }[]).map((a) => a.neg)).toEqual([1, 2]);
  });
});

describe('SubscriberPC: recovery (05 §9, 01 §10.4)', () => {
  const ICE = { pc: 'sub', gen: 1, mode: 'ice', reason: 'disconnected' };

  async function connected(): Promise<FakeRTCPeerConnection> {
    await sub.handleOffer(offer(1, 1, AV_A));
    const pc = lastPc();
    pc.setConnectionState('connected');
    return pc;
  }

  it('probes the socket at once and sends pc.restart{ice} after 3 s disconnected', async () => {
    const pc = await connected();
    const pings = server.messages('ping').length;
    pc.setConnectionState('disconnected');
    expect(server.messages('ping')).toHaveLength(pings + 1);
    expect(sub.state).toBe('reconnecting');

    await vi.advanceTimersByTimeAsync(2999);
    expect(restarts()).toEqual([]);
    await vi.advanceTimersByTimeAsync(1);
    expect(restarts()).toEqual([ICE]);
    // Once: nothing more while it stays disconnected.
    await vi.advanceTimersByTimeAsync(10_000);
    expect(restarts()).toEqual([ICE]);
  });

  it('counts iceConnectionState disconnected too', async () => {
    const pc = await connected();
    pc.setIceConnectionState('disconnected');
    await vi.advanceTimersByTimeAsync(3000);
    expect(restarts()).toEqual([ICE]);
  });

  it('cancels the 3 s timer when the PC recovers', async () => {
    const pc = await connected();
    pc.setConnectionState('disconnected');
    await vi.advanceTimersByTimeAsync(2000);
    pc.setConnectionState('connected');
    await vi.advanceTimersByTimeAsync(20_000);
    expect(restarts()).toEqual([]);
    expect(sub.state).toBe('connected');
  });

  it('sends pc.restart{rebuild, failed} at once when the PC fails', async () => {
    const pc = await connected();
    pc.setConnectionState('failed');
    expect(restarts()).toEqual([{ pc: 'sub', gen: 1, mode: 'rebuild', reason: 'failed' }]);
  });

  it('skips the ICE restart when the PC fails during the 3 s', async () => {
    const pc = await connected();
    pc.setConnectionState('disconnected');
    await vi.advanceTimersByTimeAsync(1000);
    pc.setConnectionState('failed');
    await vi.advanceTimersByTimeAsync(5000);
    expect(restarts().map((r) => r.mode)).toEqual(['rebuild']);
  });

  it('takes a sub offer with a new ice-ufrag as the ICE restart: no pc.restart{ice}, a rebuild 15 s after that offer', async () => {
    const pc = await connected();
    pc.setConnectionState('disconnected');
    await vi.advanceTimersByTimeAsync(1000);
    // The server's Resync() restarted ICE by itself (01 §10.5).
    await sub.handleOffer(offer(1, 2, AV_A, 'srv-restarted'));
    expect(pc.remoteUfrag).toBe('srv-restarted');

    await vi.advanceTimersByTimeAsync(14_999);
    expect(restarts()).toEqual([]);
    await vi.advanceTimersByTimeAsync(1);
    expect(restarts()).toEqual([{ pc: 'sub', gen: 1, mode: 'rebuild', reason: 'disconnected' }]);
  });

  it('starts the 15 s from the restart offer also when it had already sent its own request', async () => {
    const pc = await connected();
    pc.setConnectionState('disconnected');
    await vi.advanceTimersByTimeAsync(3000);
    expect(restarts()).toEqual([ICE]);
    await vi.advanceTimersByTimeAsync(1000);
    await sub.handleOffer(offer(1, 2, AV_A, 'srv-restarted'));

    // 15 s after the request, 14 s after the offer: not yet.
    await vi.advanceTimersByTimeAsync(14_000);
    expect(restarts()).toEqual([ICE]);
    await vi.advanceTimersByTimeAsync(1000);
    expect(restarts()).toEqual([ICE, { pc: 'sub', gen: 1, mode: 'rebuild', reason: 'disconnected' }]);
  });

  it('asks for no rebuild when the ICE restart connects within 15 s', async () => {
    const pc = await connected();
    pc.setConnectionState('disconnected');
    await sub.handleOffer(offer(1, 2, AV_A, 'srv-restarted'));
    await vi.advanceTimersByTimeAsync(5000);
    pc.setConnectionState('connected');
    await vi.advanceTimersByTimeAsync(60_000);
    expect(restarts()).toEqual([]);
  });

  it('does not take an offer with the same ice-ufrag for a restart', async () => {
    const pc = await connected();
    pc.setConnectionState('disconnected');
    await vi.advanceTimersByTimeAsync(1000);
    await sub.handleOffer(offer(1, 2, AV_A));
    await vi.advanceTimersByTimeAsync(2000);
    expect(restarts()).toEqual([ICE]);
  });

  it('spaces ICE restart requests 5 s apart', async () => {
    const pc = await connected();
    pc.setConnectionState('disconnected');
    await vi.advanceTimersByTimeAsync(3000); // t = 3 s: the first request
    expect(restarts()).toHaveLength(1);
    pc.setConnectionState('connected');
    await vi.advanceTimersByTimeAsync(500);
    pc.setConnectionState('disconnected'); // t = 3.5 s; its 3 s end at 6.5 s, the spacing at 8 s
    await vi.advanceTimersByTimeAsync(4499);
    expect(restarts()).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(restarts()).toEqual([ICE, ICE]);
  });

  it('spaces rebuild requests 10 s apart', async () => {
    const pc = await connected();
    pc.setConnectionState('failed'); // t = 0: rebuild of gen 1
    await vi.advanceTimersByTimeAsync(1000);
    await sub.handleOffer(offer(2, 1, AV_A, 'srv2'));
    lastPc().setConnectionState('failed'); // t = 1 s: the new PC fails too
    await vi.advanceTimersByTimeAsync(8999);
    expect(restarts()).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(restarts()).toEqual([
      { pc: 'sub', gen: 1, mode: 'rebuild', reason: 'failed' },
      { pc: 'sub', gen: 2, mode: 'rebuild', reason: 'failed' },
    ]);
  });

  it('asks again after 10 s when no new PC came and this one is still down', async () => {
    const pc = await connected();
    pc.setConnectionState('failed');
    await vi.advanceTimersByTimeAsync(9999);
    expect(restarts()).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(restarts()).toHaveLength(2);
    expect(restarts()[1]).toEqual({ pc: 'sub', gen: 1, mode: 'rebuild', reason: 'failed' });
  });

  it('reports "unreachable" after 5 rebuilds without connecting, keeps retrying every 30 s, and recovers', async () => {
    const pc = await connected();
    pc.setConnectionState('failed');
    for (let gen = 2; gen <= 5; gen++) {
      expect(viewer.store.getState().media).toBe('reconnecting');
      await sub.handleOffer(offer(gen, 1, AV_A, `srv${String(gen)}`));
      lastPc().setConnectionState('failed');
      await vi.advanceTimersByTimeAsync(10_000);
      expect(restarts()).toHaveLength(gen);
    }
    // The 5th request went out: about 40 s of trying.
    expect(sub.state).toBe('unreachable');
    expect(viewer.store.getState().media).toBe('unreachable');

    await sub.handleOffer(offer(6, 1, AV_A, 'srv6'));
    lastPc().setConnectionState('failed');
    await vi.advanceTimersByTimeAsync(29_999);
    expect(restarts()).toHaveLength(5);
    await vi.advanceTimersByTimeAsync(1);
    expect(restarts()).toHaveLength(6);
    expect(restarts()[5]).toEqual({ pc: 'sub', gen: 6, mode: 'rebuild', reason: 'failed' });

    await sub.handleOffer(offer(7, 1, AV_A, 'srv7'));
    expect(sub.state).toBe('unreachable');
    lastPc().setConnectionState('connected');
    expect(viewer.store.getState().media).toBe('connected');
    // The count starts over: the next failure is a plain reconnect again.
    await vi.advanceTimersByTimeAsync(30_000);
    lastPc().setConnectionState('failed');
    expect(restarts()).toHaveLength(7);
    expect(sub.state).toBe('reconnecting');
  });

  it('only records state changes while signaling is not ready and acts on `ready`', async () => {
    const pc = await connected();
    server.autoOpen = false;
    server.drop();
    pc.setConnectionState('disconnected');
    await vi.advanceTimersByTimeAsync(5000);
    expect(signal.state).not.toBe('ready');
    expect(restarts()).toEqual([]);

    server.accept();
    await vi.advanceTimersByTimeAsync(0);
    expect(signal.state).toBe('ready');
    expect(server.welcomes.at(-1)?.resumed).toBe(true);
    // The recorded rule (disconnected for 3 s) asks now; the server's Resync() offer will serve both (05 §9).
    expect(restarts()).toEqual([ICE]);
  });

  it('asks for the rebuild of a PC that failed while signaling was down, on `ready`', async () => {
    const pc = await connected();
    server.autoOpen = false;
    server.drop();
    pc.setConnectionState('failed');
    await vi.advanceTimersByTimeAsync(2000);
    expect(restarts()).toEqual([]);
    server.accept();
    await vi.advanceTimersByTimeAsync(0);
    expect(restarts()).toEqual([{ pc: 'sub', gen: 1, mode: 'rebuild', reason: 'failed' }]);
  });

  it('sends no pc.restart because of a resumed welcome alone', async () => {
    await sub.handleOffer(offer(1, 1, AV_A)); // still connecting: not connected
    server.drop();
    await vi.advanceTimersByTimeAsync(1000);
    expect(signal.state).toBe('ready');
    expect(server.welcomes.at(-1)?.resumed).toBe(true);
    expect(restarts()).toEqual([]);

    // Nor for a PC that has been disconnected for less than 3 s: its timer just keeps running.
    lastPc().setConnectionState('connected');
    lastPc().setConnectionState('disconnected'); // t = 0
    await vi.advanceTimersByTimeAsync(500);
    server.drop(); // the second drop in a row: back after 1 s, at t = 1.5 s
    await vi.advanceTimersByTimeAsync(1500);
    expect(signal.state).toBe('ready');
    expect(server.welcomes).toHaveLength(3);
    expect(server.welcomes.at(-1)?.resumed).toBe(true);
    expect(restarts()).toEqual([]);
    await vi.advanceTimersByTimeAsync(999);
    expect(restarts()).toEqual([]);
    await vi.advanceTimersByTimeAsync(1);
    expect(restarts()).toEqual([ICE]);
  });

  it('drops the PC after a welcome that is not resumed: gen starts at 1 again', async () => {
    await sub.handleOffer(offer(3, 1, AV_A));
    const old = lastPc();
    old.setConnectionState('connected');
    server.restart();
    server.drop();
    await vi.advanceTimersByTimeAsync(1000);
    expect(signal.state).toBe('ready');
    expect(server.welcomes.at(-1)?.resumed).toBe(false);

    expect(old.signalingState).toBe('closed');
    expect(sub.gen).toBe(0);
    expect(viewer.registry.shareIds()).toEqual([]);
    expect(viewer.store.getState().media).toBe('idle');

    await sub.handleOffer(offer(1, 1, AV_A, 'new-server'));
    expect(sub.gen).toBe(1);
    expect(lastPc()).not.toBe(old);
    expect(sent('pc.answer').at(-1)).toMatchObject({ gen: 1, neg: 1 });
  });

  it('requestRestart: nothing without a PC; with one, the request respects the spacing', async () => {
    sub.requestRestart('rebuild', 'failed');
    expect(restarts()).toEqual([]);

    await connected();
    sub.requestRestart('ice', 'disconnected');
    sub.requestRestart('ice', 'disconnected');
    expect(restarts()).toEqual([ICE]);
    sub.requestRestart('rebuild', 'disconnected');
    sub.requestRestart('rebuild', 'disconnected');
    expect(restarts()).toEqual([ICE, { pc: 'sub', gen: 1, mode: 'rebuild', reason: 'disconnected' }]);
  });
});

describe('SubscriberPC: negotiation failures (01 §9 rule 8)', () => {
  const REBUILD = { pc: 'sub', gen: 1, mode: 'rebuild', reason: 'failed' };
  const sdpInvalid = (gen = 1) => makeError('sdp_invalid', 'pc', { pc: 'sub', gen, neg: 1 });

  it('rebuilds once for sdp_invalid; a second failure within 60 s is the Fatal screen', async () => {
    await sub.handleOffer(offer(1, 1, AV_A));
    lastPc().setConnectionState('connected');
    // The server's error notification reaches the PC through the SignalClient: nobody routes it.
    server.error(sdpInvalid());
    expect(restarts()).toEqual([REBUILD]);
    expect(ui.getState().screen).toBeNull();

    await sub.handleOffer(offer(2, 1, AV_A, 'srv2'));
    await vi.advanceTimersByTimeAsync(30_000);
    server.error(makeError('bad_request', 'pc', { pc: 'sub', gen: 2, neg: 1 }));
    expect(ui.getState().screen).toEqual({ kind: 'fatal', reason: 'media' });
    expect(viewer.store.getState().media).toBe('failed');
    // Nothing more is tried.
    lastPc().setConnectionState('failed');
    await vi.advanceTimersByTimeAsync(60_000);
    expect(restarts()).toEqual([REBUILD]);
  });

  it('rebuilds again when the second failure comes after 60 s', async () => {
    await sub.handleOffer(offer(1, 1, AV_A));
    sub.handleError(sdpInvalid());
    await sub.handleOffer(offer(2, 1, AV_A, 'srv2'));
    await vi.advanceTimersByTimeAsync(60_000);
    sub.handleError(sdpInvalid(2));
    expect(restarts()).toEqual([REBUILD, { ...REBUILD, gen: 2 }]);
    expect(ui.getState().screen).toBeNull();
  });

  it('treats an offer it cannot apply the same way', async () => {
    await sub.handleOffer(offer(1, 1, AV_A));
    lastPc().failNext('setRemoteDescription', new DOMException('bad sdp', 'InvalidAccessError'));
    await sub.handleOffer(offer(1, 2, AV_A));
    expect(sent('pc.answer')).toHaveLength(1);
    expect(restarts()).toEqual([REBUILD]);
    // The same offer again gets nothing: there is no answer to replay, and the rebuild is on its way.
    await sub.handleOffer(offer(1, 2, AV_A));
    expect(sent('pc.answer')).toHaveLength(1);
    expect(restarts()).toEqual([REBUILD]);
  });

  it('ignores stale_negotiation, errors of other scopes or PCs, and errors about an older gen', async () => {
    await sub.handleOffer(offer(2, 1, AV_A));
    server.error(makeError('stale_negotiation', 'pc', { pc: 'sub', gen: 2, neg: 1 }));
    server.error(makeError('sdp_invalid', 'pc', { pc: 'pub', gen: 2, neg: 1 }));
    server.error(makeError('sdp_invalid', 'share', { shareId: 's_a' }));
    server.error(sdpInvalid(1));
    server.error(makeError('internal', 'pc', { pc: 'sub', gen: 2 }));
    sub.handleError(makeError('sdp_invalid', 'pc', { pc: 'pub', gen: 2, neg: 1 }));
    expect(restarts()).toEqual([]);
    expect(ui.getState().screen).toBeNull();
  });

  it('stops listening to errors when it closes', async () => {
    await sub.handleOffer(offer(1, 1, AV_A));
    sub.close();
    server.error(sdpInvalid());
    sub.handleError(sdpInvalid());
    expect(restarts()).toEqual([]);
    // And listens again with the next PC.
    await sub.handleOffer(offer(1, 1, AV_A));
    server.error(sdpInvalid());
    expect(restarts()).toEqual([REBUILD]);
  });

  it('repeats a rate-limited restart request after retryAfterMs', async () => {
    await sub.handleOffer(offer(1, 1, AV_A));
    lastPc().setConnectionState('connected');
    // An explicit rebuild of a connected PC: nothing but the rate-limit retry would ask again.
    sub.requestRestart('rebuild', 'failed');
    expect(restarts()).toEqual([REBUILD]);
    sub.handleError(makeError('rate_limited', 'pc', { pc: 'sub', gen: 1, retryAfterMs: 12_000 }));
    await vi.advanceTimersByTimeAsync(11_999);
    expect(restarts()).toEqual([REBUILD]);
    await vi.advanceTimersByTimeAsync(1);
    expect(restarts()).toEqual([REBUILD, REBUILD]);
  });

  it('does not repeat a rate-limited ICE restart once the PC is connected again', async () => {
    await sub.handleOffer(offer(1, 1, AV_A));
    lastPc().setConnectionState('connected');
    lastPc().setConnectionState('disconnected');
    await vi.advanceTimersByTimeAsync(3000);
    expect(restarts()).toHaveLength(1);
    sub.handleError(makeError('rate_limited', 'pc', { pc: 'sub', gen: 1 }));
    lastPc().setConnectionState('connected');
    await vi.advanceTimersByTimeAsync(30_000);
    expect(restarts()).toHaveLength(1);
  });
});

describe('SubscriberPC: close() (01 §9 rule 10: the sub PC is closed locally, without pc.close)', () => {
  it('closes the PC, sends nothing, empties the registry and stops every timer', async () => {
    await sub.handleOffer(offer(1, 1, AV_A));
    const pc = lastPc();
    pc.setConnectionState('connected');
    pc.setConnectionState('disconnected');
    const before = server.messages().length;

    sub.close();
    expect(pc.signalingState).toBe('closed');
    expect(sub.gen).toBe(0);
    expect(sub.state).toBe('idle');
    expect(viewer.store.getState().media).toBe('idle');
    expect(viewer.registry.shareIds()).toEqual([]);
    await expect(sub.getStats()).resolves.toBeNull();

    await vi.advanceTimersByTimeAsync(14_000);
    expect(sent('pc.close')).toEqual([]);
    expect(restarts()).toEqual([]);
    // Nothing but the heartbeat went out since.
    expect(
      server
        .messages()
        .slice(before)
        .every((m) => m.type === 'ping'),
    ).toBe(true);
    // Events of the closed PC are ignored.
    pc.setConnectionState('failed');
    pc.emitIceCandidate({ candidate: 'candidate:1 1 udp 1 192.0.2.1 9 typ host' });
    expect(server.messages('pc.ice')).toEqual([]);
    expect(restarts()).toEqual([]);
  });

  it('stays usable: the next sub offer, of any gen, makes a new PC', async () => {
    await sub.handleOffer(offer(4, 2, AV_A));
    sub.close();
    await sub.handleOffer(offer(1, 1, [{ mid: '0', kind: 'video', share: 's_b' }], 'other-room'));
    expect(FakeRTCPeerConnection.instances).toHaveLength(2);
    expect(sub.gen).toBe(1);
    expect(viewer.registry.shareIds()).toEqual(['s_b']);
    expect(sent('pc.answer').at(-1)).toMatchObject({ pc: 'sub', gen: 1, neg: 1 });
  });

  it('drops an offer that was waiting when it closed', async () => {
    const pending = sub.handleOffer(offer(1, 1, AV_A));
    sub.close();
    await pending;
    expect(FakeRTCPeerConnection.instances).toHaveLength(0);
    expect(sent('pc.answer')).toEqual([]);
  });

  it('abandons an offer that was being applied when it closed', async () => {
    await sub.handleOffer(offer(1, 1, AV_A));
    const pc = lastPc();
    const real = pc.setRemoteDescription.bind(pc);
    vi.spyOn(pc, 'setRemoteDescription').mockImplementation((desc) => {
      const result = real(desc);
      sub.close();
      return result;
    });
    await sub.handleOffer(offer(1, 2, AV_A));
    expect(sent('pc.answer')).toHaveLength(1);
    expect(restarts()).toEqual([]);
    expect(viewer.registry.shareIds()).toEqual([]);
    expect(ui.getState().screen).toBeNull();
  });

  it('returns the PC’s stats while it has one', async () => {
    await sub.handleOffer(offer(1, 1, AV_A));
    lastPc().stats.set('T1', { type: 'transport', dtlsState: 'connected' });
    const report = await sub.getStats();
    expect(report?.get('T1')).toEqual({ type: 'transport', dtlsState: 'connected' });
  });
});
