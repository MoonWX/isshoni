// The shared test fakes behave like the browser APIs that later slices' tests rely on.
import { afterEach, describe, expect, it, vi } from 'vitest';

import { detectCaps } from '../protocol/codecs';
import { forceOpusStereo, iceUfrag } from '../lib/sdp';
import {
  buildSdp,
  FakeRTCPeerConnection,
  installFakeRTC,
  parseSdpSections,
  type FakeRTCRtpTransceiver,
} from './FakeRTCPeerConnection';
import { FakeMediaStream, FakeMediaStreamTrack, installFakeMedia, installFakeMediaElement } from './fakeMedia';
import { createTestPlatform } from './platform';

describe('FakeRTCPeerConnection: a client-offered PC (pub)', () => {
  it('offers one m-section per transceiver with rids, applies the answer, and tracks the signaling state', async () => {
    const pc = new FakeRTCPeerConnection({ bundlePolicy: 'max-bundle' });
    const video = new FakeMediaStreamTrack('video', { width: 1920, height: 1080, displaySurface: 'window' });
    const audio = new FakeMediaStreamTrack('audio');
    const tv = pc.addTransceiver(video, {
      direction: 'sendonly',
      sendEncodings: [
        { rid: 'q', maxBitrate: 300_000 },
        { rid: 'f', maxBitrate: 8_000_000 },
      ],
    });
    pc.addTransceiver(audio, { direction: 'sendonly', sendEncodings: [{ maxBitrate: 128_000 }] });
    const offer = await pc.createOffer();
    expect(parseSdpSections(offer.sdp ?? '')).toMatchObject([
      { kind: 'video', mid: '0', direction: 'sendonly', rids: ['q', 'f'] },
      { kind: 'audio', mid: '1', direction: 'sendonly' },
    ]);
    expect(tv.mid).toBeNull(); // assigned by setLocalDescription, as in browsers
    await pc.setLocalDescription(offer);
    expect(pc.signalingState).toBe('have-local-offer');
    expect(tv.mid).toBe('0');

    const answer = buildSdp({
      setup: 'active',
      sections: [
        { kind: 'video', mid: '0', direction: 'recvonly', rids: ['q', 'f'] },
        { kind: 'audio', mid: '1', direction: 'recvonly' },
      ],
    });
    await pc.setRemoteDescription({ type: 'answer', sdp: answer });
    expect(pc.signalingState).toBe('stable');
    expect(tv.currentDirection).toBe('sendonly');
  });

  it('keeps sender parameters by position and refuses a changed encoding count', async () => {
    const pc = new FakeRTCPeerConnection();
    const t = pc.addTransceiver(new FakeMediaStreamTrack('video'), {
      sendEncodings: [{ rid: 'q' }, { rid: 'f' }],
    });
    const p = t.sender.getParameters();
    p.encodings.reverse(); // a browser may list them in another order: code must match by rid
    const f = p.encodings.find((e) => e.rid === 'f');
    if (f) f.active = false;
    await t.sender.setParameters(p);
    expect(t.sender.parameters.encodings.map((e) => [e.rid, e.active])).toEqual([
      ['f', false],
      ['q', undefined],
    ]);
    await expect(t.sender.setParameters({ ...p, encodings: [{ rid: 'q' }] })).rejects.toMatchObject({
      name: 'InvalidModificationError',
    });
  });

  it('changes the ICE ufrag on restartIce and fires negotiationneeded when stable', async () => {
    const pc = new FakeRTCPeerConnection();
    pc.addTransceiver('video', { direction: 'sendonly' });
    await Promise.resolve();
    const negotiation = vi.fn();
    pc.onnegotiationneeded = negotiation;
    const before = iceUfrag((await pc.createOffer()).sdp ?? '');
    expect(before).toBe(pc.localUfrag);
    pc.restartIce();
    await Promise.resolve();
    expect(negotiation).toHaveBeenCalledOnce();
    expect(iceUfrag((await pc.createOffer()).sdp ?? '')).not.toBe(before);
    expect(pc.restartIceCalls).toBe(1);
  });

  it('writes codec preferences into the offer', async () => {
    const pc = new FakeRTCPeerConnection();
    const t = pc.addTransceiver(new FakeMediaStreamTrack('video'), { direction: 'sendonly' });
    t.setCodecPreferences([
      { mimeType: 'video/H264', clockRate: 90000, sdpFmtpLine: 'packetization-mode=1;profile-level-id=42e01f' },
      { mimeType: 'video/rtx', clockRate: 90000 },
    ]);
    const sdp = (await pc.createOffer()).sdp ?? '';
    expect(sdp).toContain('a=rtpmap:96 H264/90000');
    expect(sdp).toContain('a=fmtp:96 packetization-mode=1;profile-level-id=42e01f');
    expect(sdp).toContain('a=fmtp:97 apt=96');
  });
});

describe('FakeRTCPeerConnection: a server-offered PC (sub)', () => {
  const offer = (ufrag: string, shares: { mid: string; kind: 'audio' | 'video'; share?: string }[]) =>
    buildSdp({
      ufrag,
      sections: shares.map((s) => ({
        kind: s.kind,
        mid: s.mid,
        direction: s.share ? 'sendonly' : 'inactive',
        ...(s.share ? { msid: { stream: s.share, track: `${s.share}-${s.kind}` } } : {}),
      })),
    });

  it('creates receiving transceivers and fires track events with the msid stream (the shareId)', async () => {
    const pc = new FakeRTCPeerConnection();
    const tracks: { mid: string | null; stream: string | undefined }[] = [];
    pc.ontrack = (ev) => {
      const e = ev as Event & { transceiver: FakeRTCRtpTransceiver; streams: FakeMediaStream[] };
      tracks.push({ mid: e.transceiver.mid, stream: e.streams[0]?.id });
    };
    await pc.setRemoteDescription({
      type: 'offer',
      sdp: offer('srv1', [
        { mid: '0', kind: 'video', share: 's_a' },
        { mid: '1', kind: 'audio', share: 's_a' },
      ]),
    });
    expect(pc.signalingState).toBe('have-remote-offer');
    expect(tracks).toEqual([
      { mid: '0', stream: 's_a' },
      { mid: '1', stream: 's_a' },
    ]);
    const answer = await pc.createAnswer();
    expect(parseSdpSections(answer.sdp ?? '').map((s) => s.direction)).toEqual(['recvonly', 'recvonly']);
    // The Opus stereo munge works on the fake's answers.
    expect(forceOpusStereo(answer.sdp ?? '')).toContain('stereo=1;sprop-stereo=1');
    await pc.setLocalDescription(answer);
    expect(pc.signalingState).toBe('stable');
  });

  it('fires track again when a reused transceiver carries a new share, and sees a new ufrag', async () => {
    const pc = new FakeRTCPeerConnection();
    const onTrack = vi.fn();
    pc.addEventListener('track', onTrack);
    await pc.setRemoteDescription({ type: 'offer', sdp: offer('u1', [{ mid: '0', kind: 'video', share: 's_a' }]) });
    await pc.setLocalDescription();
    await pc.setRemoteDescription({ type: 'offer', sdp: offer('u1', [{ mid: '0', kind: 'video' }]) }); // share ended
    await pc.setLocalDescription();
    await pc.setRemoteDescription({ type: 'offer', sdp: offer('u2', [{ mid: '0', kind: 'video', share: 's_b' }]) });
    expect(onTrack).toHaveBeenCalledTimes(2);
    expect(pc.remoteUfrag).toBe('u2');
    expect(pc.getTransceivers()).toHaveLength(1);
  });

  it('rejects candidates before a remote description (so the client must buffer them)', async () => {
    const pc = new FakeRTCPeerConnection();
    await expect(
      pc.addIceCandidate({ candidate: 'candidate:1 1 udp 1 192.0.2.1 7882 typ host' }),
    ).rejects.toMatchObject({
      name: 'InvalidStateError',
    });
    await pc.setRemoteDescription({ type: 'offer', sdp: offer('u', [{ mid: '0', kind: 'video', share: 's' }]) });
    await pc.addIceCandidate({ candidate: 'candidate:1 1 udp 1 192.0.2.1 7882 typ host', sdpMid: '0' });
    expect(pc.remoteCandidates).toHaveLength(1);
  });

  it('rejects bad transitions and every call after close()', async () => {
    const pc = new FakeRTCPeerConnection();
    await expect(pc.setRemoteDescription({ type: 'answer', sdp: '' })).rejects.toMatchObject({
      name: 'InvalidStateError',
    });
    await expect(pc.createAnswer()).rejects.toMatchObject({ name: 'InvalidStateError' });
    pc.close();
    expect(pc.connectionState).toBe('closed');
    await expect(pc.createOffer()).rejects.toMatchObject({ name: 'InvalidStateError' });
  });

  it('lets tests drive states, candidates and failures', async () => {
    const pc = new FakeRTCPeerConnection();
    const states: string[] = [];
    pc.onconnectionstatechange = () => states.push(pc.connectionState);
    pc.oniceconnectionstatechange = () => states.push(`ice:${pc.iceConnectionState}`);
    const candidates: unknown[] = [];
    pc.onicecandidate = (ev) => candidates.push((ev as Event & { candidate: unknown }).candidate);
    pc.setConnectionState('connected');
    pc.setIceConnectionState('disconnected');
    pc.emitIceCandidate({ candidate: 'c' });
    pc.emitIceCandidate(null);
    expect(states).toEqual(['connected', 'ice:disconnected']);
    expect(candidates).toEqual([{ candidate: 'c' }, null]);
    pc.failNext('createOffer', new Error('nope'));
    await expect(pc.createOffer()).rejects.toThrow('nope');
    await expect(pc.createOffer()).resolves.toMatchObject({ type: 'offer' });
  });

  it('is what the test platform creates, and every instance is listed', () => {
    const pc = createTestPlatform().createPeerConnection({});
    expect(pc).toBeInstanceOf(FakeRTCPeerConnection);
    expect(FakeRTCPeerConnection.last).toBe(pc);
  });
});

describe('FakeRTCDataChannel (the connection-test probe)', () => {
  it('opens, echoes when asked, and closes', async () => {
    const pc = new FakeRTCPeerConnection();
    const ch = pc.createDataChannel('probe', { ordered: false });
    expect(parseSdpSections((await pc.createOffer()).sdp ?? '')).toMatchObject([{ kind: 'application' }]);
    expect(() => {
      ch.send('early');
    }).toThrow();
    const got: unknown[] = [];
    ch.onmessage = (ev) => got.push(ev.data);
    ch.autoEcho = true;
    ch.open();
    ch.send('ping 1');
    await Promise.resolve();
    expect(got).toEqual(['ping 1']);
    pc.close();
    expect(ch.readyState).toBe('closed');
  });
});

describe('installFakeRTC', () => {
  let undo: (() => void) | undefined;
  afterEach(() => {
    undo?.();
    undo = undefined;
  });

  it("makes 01's detectCaps see a browser with H.264 and Opus", () => {
    undo = installFakeRTC({ displayMedia: true });
    expect(detectCaps()).toEqual({
      decode: ['h264/6400', 'h264/42e0', 'opus'],
      encode: ['h264/6400', 'h264/42e0', 'opus'],
      simulcast: true,
      displayCapture: true,
    });
  });

  it('can leave H.264 out, and undoes everything', () => {
    undo = installFakeRTC({ h264: false });
    expect(detectCaps()).toEqual({ decode: ['opus'], encode: ['opus'] });
    undo();
    undo = undefined;
    expect(detectCaps()).toEqual({ decode: [] });
    expect('mediaDevices' in navigator).toBe(false);
  });
});

describe('fake media', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('streams and tracks: settings, stop, and the ended event of a source that ends', () => {
    installFakeMedia();
    const video = new MediaStreamTrack() as unknown; // the global is the fake now
    expect(video).toBeInstanceOf(FakeMediaStreamTrack);
    const track = new FakeMediaStreamTrack('video', { displaySurface: 'monitor' });
    const stream = new FakeMediaStream([track, new FakeMediaStreamTrack('audio')]);
    expect(stream.getVideoTracks()).toEqual([track]);
    expect(track.getSettings().displaySurface).toBe('monitor');
    const ended = vi.fn();
    track.onended = ended;
    track.end();
    expect(ended).toHaveBeenCalledOnce();
    expect(track.readyState).toBe('ended');
    expect(stream.getAudioTracks()[0]?.readyState).toBe('live');
  });

  it('media elements play or refuse by policy, and keep srcObject', async () => {
    const control = installFakeMediaElement('block');
    try {
      const audio = document.createElement('audio');
      const stream = new FakeMediaStream() as unknown as MediaStream;
      audio.srcObject = stream;
      expect(audio.srcObject).toBe(stream);
      await expect(audio.play()).rejects.toMatchObject({ name: 'NotAllowedError' });
      expect(audio.paused).toBe(true);
      control.policy = 'allow';
      await audio.play();
      expect(audio.paused).toBe(false);
      audio.pause();
      expect(audio.paused).toBe(true);
      expect(control.played).toEqual([audio, audio]);
    } finally {
      control.restore();
    }
  });
});

describe('test setup (setup.ts)', () => {
  it('leaves globalThis.WebSocket assignable under MSW, so a fake signaling server can take it over', () => {
    const g = globalThis as { WebSocket?: unknown };
    const previous = g.WebSocket;
    const FakeSocket = function FakeSocket() {
      // A stand-in constructor; only its identity matters here.
    };
    try {
      g.WebSocket = FakeSocket;
      expect(g.WebSocket).toBe(FakeSocket);
    } finally {
      g.WebSocket = previous;
    }
    expect(g.WebSocket).toBe(previous);
  });
});
