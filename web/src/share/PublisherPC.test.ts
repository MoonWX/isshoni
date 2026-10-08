// PublisherPC against the real SignalClient (on the fake WebSocket server) and the fake RTCPeerConnection
// (05 §19.1): a share's transceivers (encodings, codec order, presets), the gen/neg rules of 05 §9, candidates, the
// recovery timers of the pub column, and close() (pc.close {pub}).
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { LocalError } from '../lib/errors';
import { clearLog, createLogger } from '../lib/log';
import type { Platform } from '../platform/types';
import { SignalClient } from '../protocol/signal-client';
import { FakeSignalServer, makeError } from '../protocol/testing';
import type { PCClose, PCICE, Preset, ShareParams } from '../protocol/types.gen';
import {
  FAKE_AUDIO_CODECS,
  FAKE_VIDEO_CODECS,
  FakeRTCPeerConnection,
  FakeRTCRtpSender,
  installFakeRTC,
  parseSdpSections,
} from '../test/FakeRTCPeerConnection';
import type { FakeMediaStream } from '../test/fakeMedia';
import { createTestPlatform } from '../test/platform';
import {
  DISCONNECTED_GRACE_MS,
  ICE_RESTART_SPACING_MS,
  ICE_RESTART_TIMEOUT_MS,
  MAX_BUFFERED_CANDIDATES,
  MAX_FAST_REBUILDS,
  NEGOTIATION_FAILURE_WINDOW_MS,
  PublisherPC,
  REBUILD_SLOW_SPACING_MS,
  REBUILD_SPACING_MS,
  SIZE_CHECK_MS,
  type PubMediaState,
  type PublisherPCDeps,
} from './PublisherPC';
import { captureStream } from './testing/fakeCapture';
import { answerFor, lastPc, pubOffers, shareParams } from './testing/publish';

let server: FakeSignalServer;
let signal: SignalClient;
let platform: Platform;
let pub: PublisherPC;

const capabilities: PublisherPCDeps['capabilities'] = (kind) =>
  kind === 'video' ? FAKE_VIDEO_CODECS : FAKE_AUDIO_CODECS;

const tick = async (ms = 0): Promise<void> => {
  await vi.advanceTimersByTimeAsync(ms);
};
const sent = <T>(type: string): T[] => server.messages(type).map((m) => m.data as T);
const asStream = (s: FakeMediaStream): MediaStream => s as unknown as MediaStream;

/** A window capture of 1920×1080 with sound, like getDisplayMedia's. */
const capture = (audio = true): FakeMediaStream => captureStream('window', audio);

function publisher(overrides: Partial<PublisherPCDeps> = {}): PublisherPC {
  return new PublisherPC({ platform, signal, log: createLogger('pub-pc'), capabilities, ...overrides });
}

/** Adds a share and lets the server answer its offer, so nothing is outstanding. */
async function publish(shareId = 's_a', stream = capture(), params = shareParams(shareId), preset: Preset = 'auto') {
  await pub.addShare(shareId, asStream(stream), params, preset);
  await answerLast();
  return stream;
}

async function answerLast(): Promise<void> {
  const offer = pubOffers(server).at(-1);
  if (!offer) throw new Error('no pub offer was sent');
  await pub.handleAnswer(answerFor(offer));
}

/** The network side: the PC connects. */
function connect(pc = lastPc()): void {
  pc.setIceConnectionState('connected');
  pc.setConnectionState('connected');
}

beforeEach(async () => {
  vi.useFakeTimers();
  // No jitter: the SignalClient reconnects exactly 0.5 s after the first drop (01 §10.2).
  vi.spyOn(Math, 'random').mockReturnValue(0.5);
  clearLog();
  server = FakeSignalServer.install();
  platform = createTestPlatform();
  signal = new SignalClient({
    url: platform.signaling().url,
    client: platform.client,
    role: 'full',
    caps: () => platform.capsNow(),
  });
  pub = publisher();
  signal.start();
  await tick();
  expect(signal.state).toBe('ready');
});

afterEach(() => {
  pub.close();
  signal.stop();
  server.uninstall();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe('PublisherPC: the first share', () => {
  it('makes the PC for it and sends a pub offer with gen 1, neg 1 and its tracks', async () => {
    expect(pub.gen).toBe(1);
    expect(pub.state).toBe('idle');
    expect(FakeRTCPeerConnection.instances).toHaveLength(0);

    await pub.addShare('s_a', asStream(capture()), shareParams('s_a'), 'auto');

    expect(FakeRTCPeerConnection.instances).toHaveLength(1);
    expect(lastPc().config).toEqual({ iceServers: [], bundlePolicy: 'max-bundle', rtcpMuxPolicy: 'require' });
    expect(pubOffers(server)).toEqual([
      {
        pc: 'pub',
        gen: 1,
        neg: 1,
        sdp: lastPc().localDescription?.sdp,
        tracks: [
          { mid: '0', shareId: 's_a', kind: 'video' },
          { mid: '1', shareId: 's_a', kind: 'audio' },
        ],
      },
    ]);
    expect(pub.gen).toBe(1);
    expect(pub.shareIds).toEqual(['s_a']);
    expect(pub.state).toBe('connecting');
    expect(parseSdpSections(lastPc().localDescription?.sdp ?? '').map((s) => [s.kind, s.direction])).toEqual([
      ['video', 'sendonly'],
      ['audio', 'sendonly'],
    ]);
  });

  it('uses the ICE servers of the welcome', async () => {
    server.welcomeDefaults = { iceServers: [{ urls: ['turn:turn.example:3478'], username: 'u', credential: 'c' }] };
    server.drop();
    await tick(1000);
    expect(signal.state).toBe('ready');
    await pub.addShare('s_a', asStream(capture()), shareParams('s_a'), 'auto');
    expect(lastPc().config.iceServers).toEqual([{ urls: ['turn:turn.example:3478'], username: 'u', credential: 'c' }]);
  });

  it('video: complete sendEncodings in ascending order (q, then f), built from ShareParams and the source size', async () => {
    const stream = capture();
    await pub.addShare('s_a', asStream(stream), shareParams('s_a'), 'auto');
    const [video] = lastPc().transceivers;
    expect(video?.direction).toBe('sendonly');
    expect(video?.sender.track).toBe(stream.getVideoTracks()[0]);
    // 1920×1080 = 2 073 600 px: f fits its budget (scale 1), q needs sqrt(2 073 600 / 230 400) = 3.
    expect(video?.sender.parameters.encodings).toEqual([
      { rid: 'q', active: true, maxBitrate: 300_000, maxFramerate: 15, scaleResolutionDownBy: 3 },
      { rid: 'f', active: true, maxBitrate: 8_000_000, maxFramerate: 60, scaleResolutionDownBy: 1 },
    ]);
    // Both rids are in the offer, in that order.
    expect(parseSdpSections(lastPc().localDescription?.sdp ?? '')[0]?.rids).toEqual(['q', 'f']);
  });

  it('scaleResolutionDownBy = max(1, sqrt(w·h / maxPixels)) for a source over the budget (ultrawide 3440×1440)', async () => {
    const stream = capture();
    const [track] = stream.getVideoTracks();
    if (track) track.settings = { displaySurface: 'monitor', width: 3440, height: 1440 };
    await pub.addShare('s_a', asStream(stream), shareParams('s_a'), 'auto');
    const encodings = lastPc().transceivers[0]?.sender.parameters.encodings ?? [];
    expect(encodings.map((e) => e.rid)).toEqual(['q', 'f']);
    expect(encodings[0]?.scaleResolutionDownBy).toBeCloseTo(Math.sqrt((3440 * 1440) / 230_400), 10);
    expect(encodings[1]?.scaleResolutionDownBy).toBeCloseTo(Math.sqrt((3440 * 1440) / 2_073_600), 10);
  });

  it('codec order: ShareParams.codec first, then the other H.264 profiles, then RTX; nothing else', async () => {
    await pub.addShare('s_a', asStream(capture()), shareParams('s_a', { codec: 'h264/6400' }), 'auto');
    const fmtp = (c: RTCRtpCodec): string =>
      `${c.mimeType} ${/profile-level-id=(\w+)/.exec(c.sdpFmtpLine ?? '')?.[1] ?? ''}`;
    expect(lastPc().transceivers[0]?.codecPreferences.map(fmtp)).toEqual([
      'video/H264 640034',
      'video/H264 42e01f',
      'video/rtx ',
    ]);
    // No VP8, and no packetization-mode 0, in the offer either.
    const lines = parseSdpSections(lastPc().localDescription?.sdp ?? '')[0]?.codecLines ?? [];
    expect(lines.join('\n')).not.toMatch(/VP8|packetization-mode=0/);
    expect(lines[0]).toBe('a=rtpmap:96 H264/90000');
    expect(lines[1]).toContain('profile-level-id=640034');
  });

  it('codec order: Constrained Baseline first when the room needs it (ShareParams.codec h264/42e0)', async () => {
    await pub.addShare('s_a', asStream(capture()), shareParams('s_a', { codec: 'h264/42e0' }), 'auto');
    expect(lastPc().transceivers[0]?.codecPreferences.map((c) => c.sdpFmtpLine ?? c.mimeType)).toEqual([
      expect.stringContaining('profile-level-id=42e01f'),
      expect.stringContaining('profile-level-id=640034'),
      'video/rtx',
    ]);
  });

  it('reads the capabilities from RTCRtpSender.getCapabilities by default', async () => {
    const uninstall = installFakeRTC();
    try {
      pub = publisher({ capabilities: undefined });
      await pub.addShare('s_a', asStream(capture()), shareParams('s_a'), 'auto');
      expect(lastPc().transceivers[0]?.codecPreferences.map((c) => c.mimeType)).toEqual([
        'video/H264',
        'video/H264',
        'video/rtx',
      ]);
      expect(lastPc().transceivers[1]?.codecPreferences.map((c) => c.mimeType)).toEqual(['audio/opus']);
    } finally {
      pub.close();
      uninstall();
    }
  });

  it('audio: sendonly, capped at audioBitrate, Opus only', async () => {
    const stream = capture();
    await pub.addShare('s_a', asStream(stream), shareParams('s_a', { audioBitrate: 256_000 }), 'movie');
    const audio = lastPc().transceivers[1];
    expect(audio?.kind).toBe('audio');
    expect(audio?.direction).toBe('sendonly');
    expect(audio?.sender.track).toBe(stream.getAudioTracks()[0]);
    expect(audio?.sender.parameters.encodings).toEqual([{ maxBitrate: 256_000 }]);
    expect(audio?.codecPreferences.map((c) => c.mimeType)).toEqual(['audio/opus']);
  });

  it('a capture without sound gets one m-section, and tracks names only the video', async () => {
    await pub.addShare('s_a', asStream(capture(false)), shareParams('s_a'), 'auto');
    expect(lastPc().transceivers).toHaveLength(1);
    expect(pubOffers(server)[0]?.tracks).toEqual([{ mid: '0', shareId: 's_a', kind: 'video' }]);
  });

  it.each<[Preset, string, RTCDegradationPreference]>([
    ['auto', '', 'balanced'],
    ['game', 'motion', 'maintain-framerate'],
    ['movie', 'motion', 'maintain-framerate'],
    ['text', 'text', 'maintain-resolution'],
  ])(
    'preset %s: contentHint "%s" on the track, degradationPreference %s on the sender',
    async (preset, hint, degradation) => {
      const stream = capture();
      await pub.addShare('s_a', asStream(stream), shareParams('s_a'), preset);
      expect(stream.getVideoTracks()[0]?.contentHint).toBe(hint);
      const sender = lastPc().transceivers[0]?.sender;
      // One setParameters before the offer, for degradationPreference only: the encodings are as they were given.
      expect(sender?.setParametersCalls).toHaveLength(1);
      expect(sender?.setParametersCalls[0]?.degradationPreference).toBe(degradation);
      expect(sender?.setParametersCalls[0]?.encodings.map((e) => [e.rid, e.scaleResolutionDownBy])).toEqual([
        ['q', 3],
        ['f', 1],
      ]);
      expect(pubOffers(server)).toHaveLength(1);
    },
  );

  it('a browser that rejects degradationPreference keeps its default: the offer still goes out', async () => {
    vi.spyOn(FakeRTCRtpSender.prototype, 'setParameters').mockRejectedValueOnce(
      new DOMException('no', 'InvalidModificationError'),
    );
    await pub.addShare('s_a', asStream(capture()), shareParams('s_a'), 'game');
    expect(pubOffers(server)).toHaveLength(1);
  });

  it('simulcast rejected: falls back to one encoding (f) without a rid', async () => {
    // Called with its PC below (real.call(this, …)).
    // eslint-disable-next-line @typescript-eslint/unbound-method
    const real = FakeRTCPeerConnection.prototype.addTransceiver;
    vi.spyOn(FakeRTCPeerConnection.prototype, 'addTransceiver').mockImplementation(function (
      this: FakeRTCPeerConnection,
      track,
      init,
    ) {
      if ((init?.sendEncodings?.length ?? 0) > 1) throw new TypeError('simulcast is not supported');
      return real.call(this, track, init);
    });
    await pub.addShare('s_a', asStream(capture()), shareParams('s_a'), 'auto');
    expect(lastPc().transceivers[0]?.sender.parameters.encodings).toEqual([
      { active: true, maxBitrate: 8_000_000, maxFramerate: 60, scaleResolutionDownBy: 1 },
    ]);
    expect(parseSdpSections(lastPc().localDescription?.sdp ?? '')[0]?.rids).toBeUndefined();
    expect(pubOffers(server)[0]?.tracks).toHaveLength(2);
  });

  it('no H.264 encoder at all: h264_unavailable, and neither a PC nor a message', async () => {
    pub = publisher({
      capabilities: (kind) => (kind === 'video' ? [{ mimeType: 'video/VP8', clockRate: 90000 }] : []),
    });
    const err: unknown = await pub
      .addShare('s_a', asStream(capture()), shareParams('s_a'), 'auto')
      .catch((e: unknown) => e);
    expect(err).toBeInstanceOf(LocalError);
    expect(err).toMatchObject({ code: 'h264_unavailable' });
    expect(FakeRTCPeerConnection.instances).toHaveLength(0);
    expect(pubOffers(server)).toEqual([]);
    expect(pub.shareIds).toEqual([]);
  });

  it('a stream without a video track: capture_failed', async () => {
    const stream = capture();
    for (const t of stream.getVideoTracks()) stream.removeTrack(t);
    await expect(pub.addShare('s_a', asStream(stream), shareParams('s_a'), 'auto')).rejects.toMatchObject({
      code: 'capture_failed',
    });
    expect(FakeRTCPeerConnection.instances).toHaveLength(0);
  });

  it('an offer that can’t be made: webrtc_failed, nothing of the share is left, and no pc.close for a PC nobody heard of', async () => {
    platform = createTestPlatform({
      createPeerConnection: (config) => {
        const pc = new FakeRTCPeerConnection(config);
        pc.failNext('createOffer');
        return pc as unknown as RTCPeerConnection;
      },
    });
    pub = publisher();
    await expect(pub.addShare('s_a', asStream(capture()), shareParams('s_a'), 'auto')).rejects.toMatchObject({
      code: 'webrtc_failed',
    });
    expect(pub.shareIds).toEqual([]);
    expect(pub.state).toBe('idle');
    expect(lastPc().signalingState).toBe('closed');
    expect(server.messages('pc.offer')).toEqual([]);
    expect(server.messages('pc.close')).toEqual([]);
  });

  it('refuses the same share twice', async () => {
    await publish('s_a');
    await expect(pub.addShare('s_a', asStream(capture()), shareParams('s_a'), 'auto')).rejects.toThrow(/already/);
    expect(pubOffers(server)).toHaveLength(1);
  });
});

describe('PublisherPC: gen and neg (05 §9)', () => {
  it('applies the answer to the outstanding offer, and ignores one of another gen or neg', async () => {
    await pub.addShare('s_a', asStream(capture()), shareParams('s_a'), 'auto');
    const offer = pubOffers(server)[0];
    if (!offer) throw new Error('no offer');
    await pub.handleAnswer({ ...answerFor(offer), neg: 2 });
    await pub.handleAnswer({ ...answerFor(offer), gen: 2 });
    await pub.handleAnswer({ ...answerFor(offer), pc: 'sub' });
    expect(lastPc().remoteDescription).toBeNull();
    expect(lastPc().signalingState).toBe('have-local-offer');

    await pub.handleAnswer(answerFor(offer));
    expect(lastPc().remoteDescription?.type).toBe('answer');
    expect(lastPc().signalingState).toBe('stable');
    // The same answer again (a resend after a resume): nothing is outstanding any more.
    await pub.handleAnswer(answerFor(offer));
    expect(lastPc().signalingState).toBe('stable');
  });

  it('has one outstanding offer: what changes meanwhile is folded into one follow-up offer', async () => {
    await pub.addShare('s_a', asStream(capture()), shareParams('s_a'), 'auto');
    // Two changes while offer 1 waits for its answer: a second share, and a codec switch for the first.
    await pub.addShare('s_b', asStream(capture(false)), shareParams('s_b'), 'text');
    await pub.applyParams('s_a', { codec: 'h264/42e0' });
    expect(pubOffers(server)).toHaveLength(1);

    await answerLast();
    const offers = pubOffers(server);
    expect(offers).toHaveLength(2);
    expect(offers[1]).toMatchObject({ pc: 'pub', gen: 1, neg: 2 });
    expect(offers[1]?.tracks).toEqual([
      { mid: '0', shareId: 's_a', kind: 'video' },
      { mid: '1', shareId: 's_a', kind: 'audio' },
      { mid: '2', shareId: 's_b', kind: 'video' },
    ]);
    await answerLast();
    expect(pubOffers(server)).toHaveLength(2);
  });

  it('a second share on the same PC: same gen, the next neg, the tracks of both', async () => {
    await publish('s_a');
    await pub.addShare('s_b', asStream(capture()), shareParams('s_b'), 'auto');
    expect(FakeRTCPeerConnection.instances).toHaveLength(1);
    expect(pubOffers(server)[1]).toMatchObject({ gen: 1, neg: 2 });
    expect(pubOffers(server)[1]?.tracks.map((t) => `${t.mid} ${t.shareId} ${t.kind}`)).toEqual([
      '0 s_a video',
      '1 s_a audio',
      '2 s_b video',
      '3 s_b audio',
    ]);
  });

  it('an answer that can’t be applied: one rebuild (gen 2); a second failure within 60 s ends it', async () => {
    const states: PubMediaState[] = [];
    pub.on('state', (s) => states.push(s));
    await pub.addShare('s_a', asStream(capture()), shareParams('s_a'), 'auto');
    lastPc().failNext('setRemoteDescription');
    await answerLast();
    await tick();
    expect(pub.gen).toBe(2);
    expect(FakeRTCPeerConnection.instances).toHaveLength(2);
    expect(FakeRTCPeerConnection.instances[0]?.signalingState).toBe('closed');
    expect(pubOffers(server).at(-1)).toMatchObject({
      gen: 2,
      neg: 1,
      tracks: [{ shareId: 's_a' }, { shareId: 's_a' }],
    });

    lastPc().failNext('setRemoteDescription');
    await answerLast();
    await tick();
    expect(pub.state).toBe('failed');
    expect(states.at(-1)).toBe('failed');
    expect(FakeRTCPeerConnection.instances).toHaveLength(2);
    // Nothing more is tried, whatever happens to the PC.
    lastPc().setConnectionState('failed');
    await tick(REBUILD_SLOW_SPACING_MS);
    expect(FakeRTCPeerConnection.instances).toHaveLength(2);
    await expect(pub.addShare('s_b', asStream(capture()), shareParams('s_b'), 'auto')).rejects.toMatchObject({
      code: 'webrtc_failed',
    });
  });

  it('a second negotiation failure after more than 60 s is a first one again', async () => {
    await pub.addShare('s_a', asStream(capture()), shareParams('s_a'), 'auto');
    lastPc().failNext('setRemoteDescription');
    await answerLast();
    await tick(NEGOTIATION_FAILURE_WINDOW_MS);
    lastPc().failNext('setRemoteDescription');
    await answerLast();
    await tick();
    expect(pub.state).not.toBe('failed');
    expect(pub.gen).toBe(3);
  });
});

describe('PublisherPC: removing shares and closing', () => {
  it('removeShare of one of two shares stops its transceivers and re-offers without it', async () => {
    await publish('s_a');
    await pub.addShare('s_b', asStream(capture()), shareParams('s_b'), 'auto');
    await answerLast();

    await pub.removeShare('s_a');
    expect(lastPc().transceivers.map((t) => t.stopped)).toEqual([true, true, false, false]);
    expect(pub.shareIds).toEqual(['s_b']);
    expect(pubOffers(server).at(-1)).toMatchObject({
      gen: 1,
      neg: 3,
      tracks: [
        { mid: '2', shareId: 's_b', kind: 'video' },
        { mid: '3', shareId: 's_b', kind: 'audio' },
      ],
    });
    expect(server.messages('pc.close')).toEqual([]);
  });

  it('removeShare of the last share closes the PC and sends pc.close {pub, gen}', async () => {
    const stream = await publish('s_a');
    await pub.removeShare('s_a');
    expect(lastPc().signalingState).toBe('closed');
    expect(sent<PCClose>('pc.close')).toEqual([{ pc: 'pub', gen: 1 }]);
    expect(pubOffers(server)).toHaveLength(1);
    expect(pub.state).toBe('idle');
    expect(pub.shareIds).toEqual([]);
    // The capture is its owner's to stop.
    expect(stream.getVideoTracks()[0]?.readyState).toBe('live');
  });

  it('the next share after a close gets a new PC with the next gen', async () => {
    await publish('s_a');
    await pub.removeShare('s_a');
    await pub.addShare('s_b', asStream(capture()), shareParams('s_b'), 'auto');
    expect(FakeRTCPeerConnection.instances).toHaveLength(2);
    expect(pub.gen).toBe(2);
    expect(pubOffers(server).at(-1)).toMatchObject({
      gen: 2,
      neg: 1,
      tracks: [{ mid: '0', shareId: 's_b' }, { mid: '1' }],
    });
  });

  it('close() closes the PC, sends pc.close and forgets the shares; calling it again sends nothing', async () => {
    await publish('s_a');
    pub.close();
    pub.close();
    expect(sent<PCClose>('pc.close')).toEqual([{ pc: 'pub', gen: 1 }]);
    expect(pub.shareIds).toEqual([]);
    expect(pub.state).toBe('idle');
  });

  it('removing an unknown share does nothing', async () => {
    await publish('s_a');
    await pub.removeShare('s_x');
    expect(pubOffers(server)).toHaveLength(1);
    expect(server.messages('pc.close')).toEqual([]);
  });

  it('a pc.close that couldn’t be sent goes out when signaling is back', async () => {
    await publish('s_a');
    server.drop();
    await tick();
    expect(signal.state).toBe('backoff');
    pub.close();
    expect(server.messages('pc.close')).toEqual([]);
    await tick(1000);
    expect(signal.state).toBe('ready');
    expect(sent<PCClose>('pc.close')).toEqual([{ pc: 'pub', gen: 1 }]);
  });
});

describe('PublisherPC: changes while live (05 §13.4 step 7, §13.6)', () => {
  it('applyParams {encodings}: setParameters by rid, also when the browser lists them in another order', async () => {
    await publish('s_a');
    const sender = lastPc().transceivers[0]?.sender;
    if (!sender) throw new Error('no video sender');
    // A browser may list the layers the other way round: every edit finds its encoding by rid.
    sender.parameters = { ...sender.parameters, encodings: [...sender.parameters.encodings].reverse() };
    const calls = sender.setParametersCalls.length;

    const hinted = shareParams('s_a').encodings.map((e) =>
      e.rid === 'f' ? { ...e, active: false } : { ...e, maxBitrate: 150_000 },
    );
    await pub.applyParams('s_a', { encodings: hinted });
    expect(sender.setParametersCalls).toHaveLength(calls + 1);
    expect(sender.parameters.encodings).toEqual([
      { rid: 'f', active: false, maxBitrate: 8_000_000, maxFramerate: 60, scaleResolutionDownBy: 1 },
      { rid: 'q', active: true, maxBitrate: 150_000, maxFramerate: 15, scaleResolutionDownBy: 3 },
    ]);
    // Encodings alone need no offer.
    expect(pubOffers(server)).toHaveLength(1);

    // The same list again changes nothing: no setParameters.
    await pub.applyParams('s_a', { encodings: hinted });
    expect(sender.setParametersCalls).toHaveLength(calls + 1);
  });

  it('applyParams {codec}: that profile first and a re-offer (same gen, neg + 1); the same codec again needs none', async () => {
    await publish('s_a');
    await pub.applyParams('s_a', { codec: 'h264/42e0' });
    expect(lastPc().transceivers[0]?.codecPreferences[0]?.sdpFmtpLine).toContain('profile-level-id=42e01f');
    expect(pubOffers(server).at(-1)).toMatchObject({ gen: 1, neg: 2 });
    expect(parseSdpSections(pubOffers(server).at(-1)?.sdp ?? '')[0]?.codecLines?.[1]).toContain('42e01f');
    await answerLast();

    // Hints are sent again after every resume.
    await pub.applyParams('s_a', { codec: 'h264/42e0' });
    expect(pubOffers(server)).toHaveLength(2);
  });

  it('applyParams {audioBitrate}: the sender’s cap, and a re-offer for the answer’s maxaveragebitrate', async () => {
    await publish('s_a');
    await pub.applyParams('s_a', { audioBitrate: 256_000 });
    expect(lastPc().transceivers[1]?.sender.parameters.encodings).toEqual([{ maxBitrate: 256_000 }]);
    expect(pubOffers(server).at(-1)).toMatchObject({ gen: 1, neg: 2 });
    await answerLast();
    await pub.applyParams('s_a', { audioBitrate: 256_000 });
    expect(pubOffers(server)).toHaveLength(2);
  });

  it('applyParams for a share that is gone does nothing', async () => {
    await publish('s_a');
    await pub.applyParams('s_x', { codec: 'h264/42e0', audioBitrate: 1 });
    expect(pubOffers(server)).toHaveLength(1);
  });

  it('setPreset: the new contentHint and degradationPreference, without an offer', async () => {
    const stream = await publish('s_a', capture(), shareParams('s_a'), 'auto');
    await pub.setPreset('s_a', 'text');
    expect(stream.getVideoTracks()[0]?.contentHint).toBe('text');
    expect(lastPc().transceivers[0]?.sender.parameters.degradationPreference).toBe('maintain-resolution');
    expect(pubOffers(server)).toHaveLength(1);
  });

  it('re-checks the source size every 2 s and rescales when a layer’s scale moves by more than 10 %', async () => {
    const stream = await publish('s_a');
    const [track] = stream.getVideoTracks();
    const sender = lastPc().transceivers[0]?.sender;
    if (!track || !sender) throw new Error('no video');
    const calls = sender.setParametersCalls.length;

    // 4 % wider: nothing.
    track.settings = { ...track.settings, width: 2000, height: 1080 };
    await tick(SIZE_CHECK_MS);
    expect(sender.setParametersCalls).toHaveLength(calls);

    // The window was maximised on a 1440p screen.
    track.settings = { ...track.settings, width: 2560, height: 1440 };
    await tick(SIZE_CHECK_MS);
    expect(sender.setParametersCalls).toHaveLength(calls + 1);
    const byRid = new Map(sender.parameters.encodings.map((e) => [e.rid, e.scaleResolutionDownBy]));
    expect(byRid.get('f')).toBeCloseTo(Math.sqrt((2560 * 1440) / 2_073_600), 10);
    expect(byRid.get('q')).toBe(4);
    expect(pub.targetHeight('s_a')).toBe(1080);

    // The same size at the next check: nothing.
    await tick(SIZE_CHECK_MS);
    expect(sender.setParametersCalls).toHaveLength(calls + 1);
  });

  it('shareStats reads the share’s senders', async () => {
    await publish('s_a');
    const report = new Map([['o1', { type: 'outbound-rtp', kind: 'video', rid: 'f' }]]);
    const sender = lastPc().transceivers[0]?.sender;
    if (!sender) throw new Error('no video sender');
    sender.getStats = () => Promise.resolve(report);
    const stats = await pub.shareStats('s_a');
    expect(stats.video).toBe(report);
    expect(stats.audio).toBeInstanceOf(Map);
    expect(await pub.shareStats('s_x')).toEqual({ video: null, audio: null });
  });
});

describe('PublisherPC: candidates', () => {
  it('trickles local candidates with pc.ice {pub, gen}, and the end-of-candidates marker', async () => {
    await publish('s_a');
    lastPc().emitIceCandidate({
      candidate: 'candidate:1 1 udp 1 192.0.2.1 5000 typ host',
      sdpMid: '0',
      sdpMLineIndex: 0,
    });
    lastPc().emitIceCandidate(null);
    expect(sent<PCICE>('pc.ice')).toEqual([
      {
        pc: 'pub',
        gen: 1,
        candidate: { candidate: 'candidate:1 1 udp 1 192.0.2.1 5000 typ host', sdpMid: '0', sdpMLineIndex: 0 },
      },
      { pc: 'pub', gen: 1 },
    ]);
  });

  it('keeps local candidates while signaling is down and sends them on the resumed ready', async () => {
    await publish('s_a');
    connect();
    server.drop();
    await tick();
    lastPc().emitIceCandidate({ candidate: 'candidate:2 1 udp 1 192.0.2.2 5000 typ host', sdpMid: '0' });
    expect(server.messages('pc.ice')).toEqual([]);
    await tick(1000);
    expect(sent<PCICE>('pc.ice')).toEqual([
      { pc: 'pub', gen: 1, candidate: { candidate: 'candidate:2 1 udp 1 192.0.2.2 5000 typ host', sdpMid: '0' } },
    ]);
  });

  it('buffers remote candidates until the answer is applied (at most 64, the oldest go first)', async () => {
    await pub.addShare('s_a', asStream(capture()), shareParams('s_a'), 'auto');
    for (let n = 0; n < MAX_BUFFERED_CANDIDATES + 2; n++) {
      await pub.handleIce({ pc: 'pub', gen: 1, candidate: { candidate: `candidate:${String(n)}` } });
    }
    await pub.handleIce({ pc: 'pub', gen: 0, candidate: { candidate: 'candidate:old-gen' } });
    await pub.handleIce({ pc: 'sub', gen: 1, candidate: { candidate: 'candidate:sub' } });
    expect(lastPc().remoteCandidates).toEqual([]);
    await answerLast();
    const added = lastPc().remoteCandidates.map((c) => c.candidate);
    expect(added).toHaveLength(MAX_BUFFERED_CANDIDATES);
    expect(added[0]).toBe('candidate:2');
    // With the remote description set they are added at once.
    await pub.handleIce({ pc: 'pub', gen: 1, candidate: { candidate: 'candidate:late' } });
    expect(lastPc().remoteCandidates.at(-1)).toMatchObject({ candidate: 'candidate:late', sdpMLineIndex: 0 });
  });

  it('a candidate still being added when the PC is replaced does not hold up what comes after it', async () => {
    await publish('s_a');
    // An operation on a closed RTCPeerConnection may never settle.
    vi.spyOn(lastPc(), 'addIceCandidate').mockReturnValue(new Promise<void>(() => undefined));
    const done = { ice: false, rebuild: false, remove: false };
    void pub.handleIce({ pc: 'pub', gen: 1, candidate: { candidate: 'candidate:1' } }).then(() => {
      done.ice = true;
    });
    await tick();
    expect(done.ice).toBe(false);

    // The PC is replaced from outside the queue, and the share is stopped right after.
    void pub.rebuild().then(() => {
      done.rebuild = true;
    });
    void pub.removeShare('s_a').then(() => {
      done.remove = true;
    });
    await tick();
    expect(done).toEqual({ ice: true, rebuild: true, remove: true });
    expect(pubOffers(server).at(-1)).toMatchObject({ gen: 2, neg: 1 });
    expect(sent<PCClose>('pc.close')).toEqual([{ pc: 'pub', gen: 2 }]);
    expect(pub.state).toBe('idle');
  });

  it('a candidate the PC refuses is logged and nothing more', async () => {
    await publish('s_a');
    lastPc().failNext('addIceCandidate');
    await pub.handleIce({ pc: 'pub', gen: 1, candidate: { candidate: 'candidate:bad' } });
    await pub.handleIce({ pc: 'pub', gen: 1, candidate: { candidate: 'candidate:good' } });
    expect(lastPc().remoteCandidates.map((c) => c.candidate)).toEqual(['candidate:good']);
    expect(pub.gen).toBe(1);
  });
});

describe('PublisherPC: recovery (05 §9, the pub column)', () => {
  it('reports connecting, connected and reconnecting', async () => {
    const states: PubMediaState[] = [];
    pub.on('state', (s) => states.push(s));
    await publish('s_a');
    connect();
    lastPc().setIceConnectionState('disconnected');
    connect();
    expect(states).toEqual(['connecting', 'connected', 'reconnecting', 'connected']);
  });

  it('disconnected: probes signaling at once, and restarts ICE after 3 s with a new offer of the same gen', async () => {
    await publish('s_a');
    connect();
    const pings = server.messages('ping').length;
    const ufrag = lastPc().localUfrag;

    lastPc().setIceConnectionState('disconnected');
    expect(server.messages('ping')).toHaveLength(pings + 1);
    await tick(DISCONNECTED_GRACE_MS - 1);
    expect(lastPc().restartIceCalls).toBe(0);
    await tick(1);
    expect(lastPc().restartIceCalls).toBe(1);
    expect(FakeRTCPeerConnection.instances).toHaveLength(1);
    const offer = pubOffers(server).at(-1);
    expect(offer).toMatchObject({ gen: 1, neg: 2, tracks: [{ shareId: 's_a' }, { shareId: 's_a' }] });
    expect(offer?.sdp).toContain(`a=ice-ufrag:${lastPc().localUfrag}`);
    expect(lastPc().localUfrag).not.toBe(ufrag);
  });

  it('a PC that recovers within 3 s is left alone', async () => {
    await publish('s_a');
    connect();
    lastPc().setIceConnectionState('disconnected');
    await tick(DISCONNECTED_GRACE_MS - 1);
    connect();
    await tick(ICE_RESTART_TIMEOUT_MS);
    expect(lastPc().restartIceCalls).toBe(0);
    expect(pubOffers(server)).toHaveLength(1);
  });

  it('an ICE restart that isn’t connected after 15 s: rebuild with gen + 1, the same tracks and shareIds', async () => {
    const stream = await publish('s_a');
    connect();
    lastPc().setIceConnectionState('disconnected');
    await tick(DISCONNECTED_GRACE_MS);
    await answerLast();
    await tick(ICE_RESTART_TIMEOUT_MS - 1);
    expect(FakeRTCPeerConnection.instances).toHaveLength(1);
    await tick(1);

    expect(FakeRTCPeerConnection.instances).toHaveLength(2);
    expect(FakeRTCPeerConnection.instances[0]?.signalingState).toBe('closed');
    expect(pub.gen).toBe(2);
    expect(pubOffers(server).at(-1)).toMatchObject({
      pc: 'pub',
      gen: 2,
      neg: 1,
      tracks: [
        { mid: '0', shareId: 's_a', kind: 'video' },
        { mid: '1', shareId: 's_a', kind: 'audio' },
      ],
    });
    // The same local tracks, set up like the first time; no pc.close: the higher gen replaces the server's PC.
    expect(lastPc().transceivers.map((t) => t.sender.track)).toEqual([
      stream.getVideoTracks()[0],
      stream.getAudioTracks()[0],
    ]);
    expect(lastPc().transceivers[0]?.sender.parameters.encodings.map((e) => e.rid)).toEqual(['q', 'f']);
    expect(lastPc().transceivers[0]?.codecPreferences[0]?.sdpFmtpLine).toContain('640034');
    expect(server.messages('pc.close')).toEqual([]);
    expect(pub.state).toBe('reconnecting');
  });

  it('an ICE restart that connects in time cancels the rebuild', async () => {
    await publish('s_a');
    connect();
    lastPc().setIceConnectionState('disconnected');
    await tick(DISCONNECTED_GRACE_MS);
    await answerLast();
    connect();
    await tick(ICE_RESTART_TIMEOUT_MS);
    expect(FakeRTCPeerConnection.instances).toHaveLength(1);
    expect(pub.state).toBe('connected');
  });

  it('at most one ICE restart per 5 s', async () => {
    await publish('s_a');
    connect();
    lastPc().setIceConnectionState('disconnected');
    await tick(DISCONNECTED_GRACE_MS);
    await answerLast();
    connect();
    // 1 s after the restart it drops again: the next restart waits for the spacing, not just for the 3 s.
    await tick(1000);
    lastPc().setIceConnectionState('disconnected');
    await tick(DISCONNECTED_GRACE_MS);
    expect(lastPc().restartIceCalls).toBe(1);
    await tick(ICE_RESTART_SPACING_MS - 1000 - DISCONNECTED_GRACE_MS);
    expect(lastPc().restartIceCalls).toBe(2);
  });

  it('a failed PC is rebuilt at once, then at most once per 10 s', async () => {
    await publish('s_a');
    connect();
    lastPc().setConnectionState('failed');
    await tick();
    expect(pub.gen).toBe(2);
    expect(pubOffers(server).at(-1)).toMatchObject({ gen: 2, neg: 1 });

    lastPc().setConnectionState('failed');
    await tick(REBUILD_SPACING_MS - 1);
    expect(pub.gen).toBe(2);
    await tick(1);
    expect(pub.gen).toBe(3);
    expect(pubOffers(server).at(-1)).toMatchObject({ gen: 3, neg: 1 });
  });

  it('5 rebuilds without connecting: unreachable, and rebuilds are 30 s apart; connecting ends it', async () => {
    await publish('s_a');
    connect();
    for (let n = 1; n <= MAX_FAST_REBUILDS; n++) {
      expect(pub.state).not.toBe('unreachable');
      lastPc().setConnectionState('failed');
      await tick();
      expect(pub.gen).toBe(1 + n);
      if (n < MAX_FAST_REBUILDS) await tick(REBUILD_SPACING_MS);
    }
    expect(pub.state).toBe('unreachable');
    const gen = pub.gen;

    // The next one waits 30 s from that rebuild.
    lastPc().setConnectionState('failed');
    await tick(REBUILD_SLOW_SPACING_MS - 1);
    expect(pub.gen).toBe(gen);
    await tick(1);
    expect(pub.gen).toBe(gen + 1);

    connect();
    expect(pub.state).toBe('connected');
  });

  it('the server’s pc.restart {pub, rebuild} rebuilds; one for an older gen is ignored', async () => {
    await publish('s_a');
    connect();
    await pub.handleRestart({ pc: 'pub', gen: 1, mode: 'rebuild', reason: 'failed' });
    await tick();
    expect(pub.gen).toBe(2);
    expect(pubOffers(server).at(-1)).toMatchObject({ gen: 2, neg: 1 });

    await tick(REBUILD_SPACING_MS);
    await pub.handleRestart({ pc: 'pub', gen: 1, mode: 'rebuild', reason: 'failed' });
    await pub.handleRestart({ pc: 'sub', gen: 2, mode: 'rebuild', reason: 'failed' });
    await tick();
    expect(pub.gen).toBe(2);
  });

  it('the server’s pc.restart {pub, ice} restarts ICE', async () => {
    await publish('s_a');
    connect();
    await pub.handleRestart({ pc: 'pub', gen: 1, mode: 'ice', reason: 'disconnected' });
    expect(lastPc().restartIceCalls).toBe(1);
    expect(pubOffers(server).at(-1)).toMatchObject({ gen: 1, neg: 2 });
  });

  it('a rebuild the server asked for waits for the spacing, and this side connecting meanwhile doesn’t settle it', async () => {
    await publish('s_a');
    connect();
    await pub.rebuild();
    await answerLast();
    expect(pub.gen).toBe(2);

    // The server's side of gen 2 failed (its handshake timer); this side only just reports connected.
    await pub.handleRestart({ pc: 'pub', gen: 2, mode: 'rebuild', reason: 'failed' });
    connect();
    await tick(REBUILD_SPACING_MS - 1);
    expect(pub.gen).toBe(2);
    await tick(1);
    expect(pub.gen).toBe(3);
    expect(pubOffers(server).at(-1)).toMatchObject({ gen: 3, neg: 1 });
  });

  it('rebuild(): gen + 1 at once; a no-op without a share', async () => {
    await pub.rebuild();
    expect(FakeRTCPeerConnection.instances).toHaveLength(0);
    await publish('s_a');
    await pub.rebuild();
    expect(pub.gen).toBe(2);
    expect(pubOffers(server).at(-1)).toMatchObject({
      gen: 2,
      neg: 1,
      tracks: [{ shareId: 's_a' }, { shareId: 's_a' }],
    });
  });

  it('sdp_invalid about the pub offer: one rebuild; again within 60 s: failed. Other PCs and older gens don’t count', async () => {
    await pub.addShare('s_a', asStream(capture()), shareParams('s_a'), 'auto');
    pub.handleError(makeError('sdp_invalid', 'pc', { pc: 'sub', gen: 1, neg: 1 }));
    pub.handleError(makeError('sdp_invalid', 'request', { pc: 'pub', gen: 1, neg: 1 }));
    await tick();
    expect(pub.gen).toBe(1);

    pub.handleError(makeError('sdp_invalid', 'pc', { pc: 'pub', gen: 1, neg: 1 }));
    await tick();
    expect(pub.gen).toBe(2);
    expect(pubOffers(server).at(-1)).toMatchObject({ gen: 2, neg: 1 });

    // About the PC that is gone.
    pub.handleError(makeError('bad_request', 'pc', { pc: 'pub', gen: 1, neg: 1 }));
    await tick();
    expect(pub.state).toBe('connecting');

    pub.handleError(makeError('bad_request', 'pc', { pc: 'pub', gen: 2, neg: 1 }));
    await tick();
    expect(pub.state).toBe('failed');
    expect(pub.gen).toBe(2);
  });

  it('stale_negotiation is ignored, and unknown codes are only logged', async () => {
    await pub.addShare('s_a', asStream(capture()), shareParams('s_a'), 'auto');
    pub.handleError(makeError('stale_negotiation', 'pc', { pc: 'pub', gen: 1, neg: 1 }));
    pub.handleError(makeError('something_new', 'pc', { pc: 'pub', gen: 1 }));
    await tick();
    expect(pub.gen).toBe(1);
    await answerLast();
    expect(lastPc().signalingState).toBe('stable');
  });

  it('rate_limited about the outstanding offer: the same offer goes out again after retryAfterMs', async () => {
    await pub.addShare('s_a', asStream(capture()), shareParams('s_a'), 'auto');
    pub.handleError(makeError('rate_limited', 'pc', { pc: 'pub', gen: 1, neg: 1, retryAfterMs: 2000 }));
    await tick(1999);
    expect(pubOffers(server)).toHaveLength(1);
    await tick(1);
    expect(pubOffers(server)).toHaveLength(2);
    expect(pubOffers(server)[1]).toEqual(pubOffers(server)[0]);
  });
});

describe('PublisherPC: signaling drops (01 §10.4, §10.5)', () => {
  it('an offer made while signaling is down goes out on the resumed ready, with the same neg', async () => {
    await publish('s_a');
    server.drop();
    await tick();
    await pub.applyParams('s_a', { codec: 'h264/42e0' });
    expect(pubOffers(server)).toHaveLength(1);
    await tick(1000);
    expect(signal.state).toBe('ready');
    expect(pubOffers(server).at(-1)).toMatchObject({ gen: 1, neg: 2 });
    expect(pubOffers(server)).toHaveLength(2);
  });

  it('the outstanding offer is sent again after a resume: its answer may have been lost', async () => {
    await pub.addShare('s_a', asStream(capture()), shareParams('s_a'), 'auto');
    server.drop();
    await tick(1000);
    expect(signal.state).toBe('ready');
    const offers = pubOffers(server);
    expect(offers).toHaveLength(2);
    expect(offers[1]).toEqual(offers[0]);
    // The PC isn't connected, but it waits for that answer: nothing is wrong with its ICE yet.
    expect(lastPc().restartIceCalls).toBe(0);
    await answerLast();
    expect(lastPc().signalingState).toBe('stable');
  });

  it('while signaling is down a disconnected PC is only recorded; ICE restarts on the resumed ready', async () => {
    await publish('s_a');
    connect();
    server.drop();
    await tick();
    lastPc().setIceConnectionState('disconnected');
    await tick(400);
    expect(lastPc().restartIceCalls).toBe(0);
    // Ready again 0.5 s after the drop: the restart doesn't wait for the 3 s.
    await tick(100);
    expect(signal.state).toBe('ready');
    expect(lastPc().restartIceCalls).toBe(1);
    expect(pubOffers(server).at(-1)).toMatchObject({ gen: 1, neg: 2 });
  });

  it('a PC that is still connecting although its offer was answered restarts ICE on the resumed ready too', async () => {
    // The network changed under the first checks: the answer is applied, and ICE never gets anywhere.
    await publish('s_a');
    lastPc().setConnectionState('connecting');
    lastPc().setIceConnectionState('checking');
    expect(pub.state).toBe('connecting');
    server.drop();
    await tick(1000);
    expect(signal.state).toBe('ready');
    expect(signal.welcome?.resumed).toBe(true);

    expect(lastPc().restartIceCalls).toBe(1);
    expect(FakeRTCPeerConnection.instances).toHaveLength(1);
    expect(pubOffers(server).at(-1)).toMatchObject({ gen: 1, neg: 2 });
    // From here it is an ICE restart like any other: not connected after 15 s → rebuild.
    await answerLast();
    await tick(ICE_RESTART_TIMEOUT_MS);
    expect(pub.gen).toBe(2);
  });

  it('an ICE restart that is under way when signaling resumes is left to its 15 s timer', async () => {
    await publish('s_a');
    connect();
    lastPc().setIceConnectionState('disconnected');
    await tick(DISCONNECTED_GRACE_MS);
    await answerLast();
    expect(lastPc().restartIceCalls).toBe(1);

    server.drop();
    await tick(1000);
    expect(signal.state).toBe('ready');
    // Also once the 5 s spacing is over: no second restart was wanted.
    await tick(ICE_RESTART_SPACING_MS);
    expect(lastPc().restartIceCalls).toBe(1);
    expect(pubOffers(server)).toHaveLength(2);
    expect(pub.gen).toBe(1);
  });

  it('a failure while signaling is down is acted on at ready', async () => {
    await publish('s_a');
    connect();
    server.drop();
    await tick();
    lastPc().setConnectionState('failed');
    await tick(400);
    expect(pub.gen).toBe(1);
    await tick(100);
    expect(pub.gen).toBe(2);
    expect(pubOffers(server).at(-1)).toMatchObject({ gen: 2, neg: 1 });
  });

  it('a welcome that is not resumed: the PC is dropped without a message, and gen starts at 1 again', async () => {
    const states: PubMediaState[] = [];
    await publish('s_a');
    connect();
    await pub.rebuild();
    expect(pub.gen).toBe(2);
    pub.on('state', (s) => states.push(s));

    server.restart();
    server.drop();
    await tick(1000);
    expect(signal.state).toBe('ready');
    expect(signal.welcome?.resumed).toBe(false);
    expect(lastPc().signalingState).toBe('closed');
    expect(server.messages('pc.close')).toEqual([]);
    expect(pub.state).toBe('idle');
    expect(states).toEqual(['idle']);
    expect(pub.gen).toBe(1);
    // The share is still known here (parked) until its owner removes it or publishes it again.
    expect(pub.shareIds).toEqual(['s_a']);

    // Its owner stops it: nothing to close, nothing to send.
    await pub.removeShare('s_a');
    expect(server.messages('pc.close')).toEqual([]);
    // A new share starts over at gen 1.
    await pub.addShare('s_b', asStream(capture()), shareParams('s_b'), 'auto');
    expect(pubOffers(server).at(-1)).toMatchObject({
      gen: 1,
      neg: 1,
      tracks: [{ shareId: 's_b' }, { shareId: 's_b' }],
    });
  });

  it('gen starts at 1 again also when no PC exists at that welcome: the last share was stopped before it', async () => {
    await publish('s_a');
    await pub.removeShare('s_a');
    expect(sent<PCClose>('pc.close')).toEqual([{ pc: 'pub', gen: 1 }]);

    server.restart();
    server.drop();
    await tick(1000);
    expect(signal.state).toBe('ready');
    expect(signal.welcome?.resumed).toBe(false);

    // On the same connection this would be gen 2; on the new one the server starts counting at 1 (01 §9 rule 2).
    await pub.addShare('s_b', asStream(capture()), shareParams('s_b'), 'auto');
    expect(pub.gen).toBe(1);
    expect(pubOffers(server).at(-1)).toMatchObject({
      gen: 1,
      neg: 1,
      tracks: [{ shareId: 's_b' }, { shareId: 's_b' }],
    });
  });

  it('a resumed welcome between two shares changes nothing: the next PC has the next gen', async () => {
    await publish('s_a');
    await pub.removeShare('s_a');
    server.drop();
    await tick(1000);
    expect(signal.welcome?.resumed).toBe(true);
    await pub.addShare('s_b', asStream(capture()), shareParams('s_b'), 'auto');
    expect(pubOffers(server).at(-1)).toMatchObject({ gen: 2, neg: 1 });
  });

  it('after that welcome, rebuild() puts the parked shares on a new PC of gen 1, set up as before', async () => {
    const stream = await publish('s_a');
    connect();
    server.restart();
    server.drop();
    await tick(1000);
    expect(pub.state).toBe('idle');

    await pub.rebuild();
    expect(pub.gen).toBe(1);
    expect(pubOffers(server).at(-1)).toMatchObject({
      gen: 1,
      neg: 1,
      tracks: [
        { mid: '0', shareId: 's_a', kind: 'video' },
        { mid: '1', shareId: 's_a', kind: 'audio' },
      ],
    });
    // The size check runs for it again.
    const [track] = stream.getVideoTracks();
    const sender = lastPc().transceivers[0]?.sender;
    if (!track || !sender) throw new Error('no video');
    track.settings = { ...track.settings, width: 3840, height: 2160 };
    await tick(SIZE_CHECK_MS);
    expect(new Map(sender.parameters.encodings.map((e) => [e.rid, e.scaleResolutionDownBy])).get('f')).toBe(2);
  });
});

describe('PublisherPC with only ShareContext.signal’s structural type', () => {
  it('works without state, welcome and onState: signaling counts as ready, ICE servers as none', async () => {
    const notify = vi.fn(() => true);
    const bare = publisher({
      signal: { request: vi.fn(), notify, on: () => () => undefined, probe: vi.fn() },
    });
    const params: ShareParams = shareParams('s_a');
    await bare.addShare('s_a', asStream(capture()), params, 'auto');
    expect(lastPc().config.iceServers).toEqual([]);
    expect(notify).toHaveBeenCalledWith('pc.offer', expect.objectContaining({ pc: 'pub', gen: 1, neg: 1 }));
    lastPc().setConnectionState('failed');
    await tick();
    expect(bare.gen).toBe(2);
    bare.close();
    expect(notify).toHaveBeenLastCalledWith('pc.close', { pc: 'pub', gen: 2 });
  });
});
