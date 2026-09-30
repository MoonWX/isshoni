// A fake RTCPeerConnection for Vitest (jsdom has no WebRTC). README §4 Testing: the controllers (SubscriberPC,
// PublisherPC, the connection-test probes) run against it through platform.createPeerConnection
// (test/platform.ts), and the tests drive the network side by hand.
//
// What it models:
// - the signaling state machine (stable ⇄ have-local-offer / have-remote-offer, rollback, closed), rejecting bad
//   transitions with InvalidStateError like browsers do;
// - SDP text: createOffer writes one m-section per transceiver (codecs from setCodecPreferences, rids and
//   a=simulcast for sendEncodings with rids, a=ice-ufrag that restartIce() changes); createAnswer mirrors the remote
//   offer with the directions flipped. buildSdp() writes SDP for tests (a server's sub offer);
// - transceivers created by a remote offer, and `track` events when a remote m-section starts sending (or changes
//   its msid), with one receiver track per transceiver (transceivers are reused, 02);
// - addIceCandidate rejecting before a remote description (so buffering is testable);
// - senders' getParameters/setParameters (encodings by position, a changed count is InvalidModificationError);
// - data channels (FakeRTCDataChannel) that tests open, feed and close, optionally echoing (the conntest probe).
// Test controls: setConnectionState, setIceConnectionState, emitIceCandidate, failNext, stats; every instance is in
// FakeRTCPeerConnection.instances (cleared after each test by setup.ts).
import { vi } from 'vitest';

import { FakeMediaStream, FakeMediaStreamTrack } from './fakeMedia';

type Direction = RTCRtpTransceiverDirection;
type Kind = 'audio' | 'video' | 'application';

/** One m-section of an SDP, as buildSdp writes it and the fake parses it. */
export interface SdpSection {
  kind: Kind;
  mid: string;
  direction: Direction;
  /** a=msid:<stream> <track>; the stream id is the shareId on isshoni's sub PC (02). */
  msid?: { stream: string; track: string };
  /** a=rid:<rid> send|recv and a=simulcast. */
  rids?: string[];
  /** Codec lines (a=rtpmap/a=fmtp/a=rtcp-fb) to use instead of the defaults for the kind. */
  codecLines?: string[];
}

export interface BuildSdpOptions {
  ufrag?: string;
  /** actpass for offers, active for answers. */
  setup?: 'actpass' | 'active' | 'passive';
  sections: SdpSection[];
}

const DEFAULT_CODEC_LINES: Readonly<Record<Kind, readonly string[]>> = {
  video: [
    'a=rtpmap:96 H264/90000',
    'a=rtcp-fb:96 nack',
    'a=rtcp-fb:96 nack pli',
    'a=fmtp:96 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=640c1f',
    'a=rtpmap:97 rtx/90000',
    'a=fmtp:97 apt=96',
    'a=rtpmap:98 H264/90000',
    'a=fmtp:98 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f',
    'a=rtpmap:99 rtx/90000',
    'a=fmtp:99 apt=98',
  ],
  audio: ['a=rtpmap:111 opus/48000/2', 'a=fmtp:111 minptime=10;useinbandfec=1'],
  application: ['a=sctp-port:5000'],
};

let counter = 0;
const nextToken = (prefix: string): string => `${prefix}${String(++counter).padStart(4, '0')}`;

function payloadTypes(lines: readonly string[]): string {
  const pts = lines.flatMap((l) => /^a=rtpmap:(\d+) /.exec(l)?.[1] ?? []);
  return pts.length > 0 ? pts.join(' ') : '0';
}

/** Writes an SDP with the given m-sections (CRLF line ends, max-bundle). */
export function buildSdp({ ufrag = nextToken('uf'), setup = 'actpass', sections }: BuildSdpOptions): string {
  const lines = ['v=0', `o=- ${String(1000 + counter)} 2 IN IP4 127.0.0.1`, 's=-', 't=0 0'];
  if (sections.length > 0) lines.push(`a=group:BUNDLE ${sections.map((s) => s.mid).join(' ')}`);
  lines.push('a=msid-semantic: WMS');
  for (const s of sections) {
    const codecs = s.codecLines ?? DEFAULT_CODEC_LINES[s.kind];
    if (s.kind === 'application') {
      lines.push('m=application 9 UDP/DTLS/SCTP webrtc-datachannel');
    } else {
      lines.push(`m=${s.kind} 9 UDP/TLS/RTP/SAVPF ${payloadTypes(codecs)}`);
    }
    lines.push(
      'c=IN IP4 0.0.0.0',
      `a=ice-ufrag:${ufrag}`,
      'a=ice-pwd:fakepasswordfakepassword00',
      'a=fingerprint:sha-256 00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF',
      `a=setup:${setup}`,
      `a=mid:${s.mid}`,
    );
    if (s.kind !== 'application') {
      lines.push(`a=${s.direction}`);
      if (s.msid) lines.push(`a=msid:${s.msid.stream} ${s.msid.track}`);
      lines.push('a=rtcp-mux');
    }
    lines.push(...codecs);
    if (s.rids && s.rids.length > 0) {
      const way = s.direction === 'recvonly' ? 'recv' : 'send';
      for (const rid of s.rids) lines.push(`a=rid:${rid} ${way}`);
      lines.push(`a=simulcast:${way} ${s.rids.join(';')}`);
    }
  }
  return lines.join('\r\n') + '\r\n';
}

/** Splits an SDP into m-sections: kind, mid, direction, msid, rids and codec lines. */
export function parseSdpSections(sdp: string): SdpSection[] {
  const out: SdpSection[] = [];
  let cur: SdpSection | null = null;
  for (const line of sdp.split(/\r?\n/)) {
    const m = /^m=(audio|video|application) /.exec(line);
    if (m?.[1] !== undefined) {
      cur = { kind: m[1] as Kind, mid: '', direction: 'sendrecv', codecLines: [] };
      out.push(cur);
      continue;
    }
    if (!cur) continue;
    let v: RegExpExecArray | null;
    if ((v = /^a=mid:(\S+)/.exec(line)) && v[1] !== undefined) cur.mid = v[1];
    else if (/^a=(sendrecv|sendonly|recvonly|inactive)$/.test(line)) cur.direction = line.slice(2) as Direction;
    else if ((v = /^a=msid:(\S+) (\S+)/.exec(line)) && v[1] !== undefined && v[2] !== undefined) {
      cur.msid = { stream: v[1], track: v[2] };
    } else if ((v = /^a=rid:(\S+) /.exec(line)) && v[1] !== undefined) (cur.rids ??= []).push(v[1]);
    else if (/^a=(rtpmap|fmtp|rtcp-fb|sctp-port):/.test(line)) cur.codecLines?.push(line);
  }
  return out;
}

/** The a=ice-ufrag of an SDP. */
function ufragOf(sdp: string): string | null {
  return /^a=ice-ufrag:(\S+)/m.exec(sdp)?.[1] ?? null;
}

const FLIP: Readonly<Record<Direction, Direction>> = {
  sendrecv: 'sendrecv',
  sendonly: 'recvonly',
  recvonly: 'sendonly',
  inactive: 'inactive',
  stopped: 'inactive',
};

function invalidState(msg: string): DOMException {
  return new DOMException(msg, 'InvalidStateError');
}

// ---- senders, receivers, transceivers ----

export class FakeRTCRtpSender {
  track: FakeMediaStreamTrack | null;
  /** What getParameters() copies from; setParameters() replaces it. Tests may reorder it. */
  parameters: RTCRtpSendParameters;
  /** Every setParameters() call's argument, in order. */
  readonly setParametersCalls: RTCRtpSendParameters[] = [];

  constructor(track: FakeMediaStreamTrack | null, encodings: RTCRtpEncodingParameters[] = [{}]) {
    this.track = track;
    this.parameters = {
      transactionId: nextToken('tx'),
      encodings: encodings.map((e) => ({ ...e })),
      headerExtensions: [],
      rtcp: {},
      codecs: [],
    };
  }

  getParameters(): RTCRtpSendParameters {
    return structuredClone(this.parameters);
  }

  setParameters(p: RTCRtpSendParameters): Promise<void> {
    this.setParametersCalls.push(structuredClone(p));
    if (p.encodings.length !== this.parameters.encodings.length) {
      return Promise.reject(new DOMException('encodings count changed', 'InvalidModificationError'));
    }
    this.parameters = { ...structuredClone(p), transactionId: nextToken('tx') };
    return Promise.resolve();
  }

  replaceTrack(track: FakeMediaStreamTrack | null): Promise<void> {
    this.track = track;
    return Promise.resolve();
  }

  getStats(): Promise<Map<string, unknown>> {
    return Promise.resolve(new Map<string, unknown>());
  }
}

export class FakeRTCRtpReceiver {
  readonly track: FakeMediaStreamTrack;

  constructor(kind: 'audio' | 'video') {
    this.track = new FakeMediaStreamTrack(kind, {}, `remote ${kind}`);
  }

  getStats(): Promise<Map<string, unknown>> {
    return Promise.resolve(new Map<string, unknown>());
  }
}

export class FakeRTCRtpTransceiver {
  mid: string | null = null;
  direction: Direction;
  currentDirection: Direction | null = null;
  stopped = false;
  readonly kind: 'audio' | 'video';
  readonly sender: FakeRTCRtpSender;
  readonly receiver: FakeRTCRtpReceiver;
  /** The last setCodecPreferences() argument ([] = the browser's default order). */
  codecPreferences: RTCRtpCodec[] = [];
  /** Remote side: whether it sends, and with which msid, as of the last remote description. */
  remoteSending = false;
  remoteMsid: string | null = null;

  constructor(
    kind: 'audio' | 'video',
    direction: Direction,
    track: FakeMediaStreamTrack | null,
    encodings?: RTCRtpEncodingParameters[],
  ) {
    this.kind = kind;
    this.direction = direction;
    this.sender = new FakeRTCRtpSender(track, encodings);
    this.receiver = new FakeRTCRtpReceiver(kind);
  }

  setCodecPreferences(codecs: readonly RTCRtpCodec[]): void {
    this.codecPreferences = codecs.map((c) => ({ ...c }));
  }

  stop(): void {
    this.stopped = true;
    this.direction = 'stopped';
    this.currentDirection = 'stopped';
  }
}

// ---- data channels ----

export class FakeRTCDataChannel extends EventTarget {
  readonly label: string;
  readonly id: number | null;
  readonly ordered: boolean;
  readyState: RTCDataChannelState = 'connecting';
  binaryType: BinaryType = 'arraybuffer';
  bufferedAmount = 0;
  /** Everything send() got, in order. */
  readonly sent: unknown[] = [];
  /** Test control: answer every send() with the same data, like the connection-test probe's echo (02 §7.6). */
  autoEcho = false;
  onopen: ((ev: Event) => unknown) | null = null;
  onmessage: ((ev: MessageEvent) => unknown) | null = null;
  onclose: ((ev: Event) => unknown) | null = null;
  onerror: ((ev: Event) => unknown) | null = null;

  constructor(label: string, init: RTCDataChannelInit = {}) {
    super();
    this.label = label;
    this.id = init.id ?? null;
    this.ordered = init.ordered ?? true;
  }

  send(data: unknown): void {
    if (this.readyState !== 'open') throw invalidState('data channel is not open');
    this.sent.push(data);
    if (this.autoEcho) {
      queueMicrotask(() => {
        this.receive(data);
      });
    }
  }

  close(): void {
    if (this.readyState === 'closed') return;
    this.readyState = 'closed';
    this.#fire('close', new Event('close'));
  }

  /** Test control: the channel opened. */
  open(): void {
    this.readyState = 'open';
    this.#fire('open', new Event('open'));
  }

  /** Test control: a message from the other side. */
  receive(data: unknown): void {
    this.#fire('message', new MessageEvent('message', { data }));
  }

  #fire(type: 'open' | 'message' | 'close' | 'error', ev: Event): void {
    const handler = this[`on${type}`] as ((ev: Event) => unknown) | null;
    handler?.call(this, ev);
    this.dispatchEvent(ev);
  }
}

// ---- the peer connection ----

type Handler = ((ev: Event) => unknown) | null;
type FailableMethod =
  'createOffer' | 'createAnswer' | 'setLocalDescription' | 'setRemoteDescription' | 'addIceCandidate';

export class FakeRTCPeerConnection extends EventTarget {
  /** Every instance created since the last reset(), in order. */
  static instances: FakeRTCPeerConnection[] = [];

  static reset(): void {
    FakeRTCPeerConnection.instances = [];
  }

  /** The newest instance (the one the code under test just made). */
  static get last(): FakeRTCPeerConnection | undefined {
    return FakeRTCPeerConnection.instances.at(-1);
  }

  config: RTCConfiguration;
  signalingState: RTCSignalingState = 'stable';
  connectionState: RTCPeerConnectionState = 'new';
  iceConnectionState: RTCIceConnectionState = 'new';
  iceGatheringState: RTCIceGatheringState = 'new';
  localDescription: RTCSessionDescriptionInit | null = null;
  remoteDescription: RTCSessionDescriptionInit | null = null;
  readonly transceivers: FakeRTCRtpTransceiver[] = [];
  readonly dataChannels: FakeRTCDataChannel[] = [];
  /** Candidates given to addIceCandidate, in order. */
  readonly remoteCandidates: RTCIceCandidateInit[] = [];
  /** How often restartIce() was called. */
  restartIceCalls = 0;
  /** What getStats() resolves with (a Map, like RTCStatsReport). */
  stats = new Map<string, Record<string, unknown>>();

  onicecandidate: Handler = null;
  ontrack: Handler = null;
  onconnectionstatechange: Handler = null;
  oniceconnectionstatechange: Handler = null;
  onicegatheringstatechange: Handler = null;
  onsignalingstatechange: Handler = null;
  onnegotiationneeded: Handler = null;
  ondatachannel: Handler = null;

  #ufrag = nextToken('lf');
  #nextMid = 0;
  #failures = new Map<FailableMethod, unknown>();
  #negotiationQueued = false;

  constructor(config: RTCConfiguration = {}) {
    super();
    this.config = { ...config };
    FakeRTCPeerConnection.instances.push(this);
  }

  getConfiguration(): RTCConfiguration {
    return { ...this.config };
  }

  setConfiguration(config: RTCConfiguration): void {
    this.config = { ...config };
  }

  // ---- media ----

  addTransceiver(
    trackOrKind: FakeMediaStreamTrack | 'audio' | 'video',
    init: RTCRtpTransceiverInit = {},
  ): FakeRTCRtpTransceiver {
    this.#assertOpen();
    const track = typeof trackOrKind === 'string' ? null : trackOrKind;
    const kind = typeof trackOrKind === 'string' ? trackOrKind : trackOrKind.kind;
    const t = new FakeRTCRtpTransceiver(kind, init.direction ?? 'sendrecv', track, init.sendEncodings);
    this.transceivers.push(t);
    this.#negotiationNeeded();
    return t;
  }

  addTrack(track: FakeMediaStreamTrack): FakeRTCRtpSender {
    return this.addTransceiver(track, { direction: 'sendrecv' }).sender;
  }

  removeTrack(sender: FakeRTCRtpSender): void {
    const t = this.transceivers.find((x) => x.sender === sender);
    if (!t) return;
    sender.track = null;
    t.direction = t.direction === 'sendrecv' ? 'recvonly' : 'inactive';
    this.#negotiationNeeded();
  }

  getTransceivers(): FakeRTCRtpTransceiver[] {
    return [...this.transceivers];
  }

  getSenders(): FakeRTCRtpSender[] {
    return this.transceivers.filter((t) => !t.stopped).map((t) => t.sender);
  }

  getReceivers(): FakeRTCRtpReceiver[] {
    return this.transceivers.filter((t) => !t.stopped).map((t) => t.receiver);
  }

  createDataChannel(label: string, init?: RTCDataChannelInit): FakeRTCDataChannel {
    this.#assertOpen();
    const ch = new FakeRTCDataChannel(label, init);
    this.dataChannels.push(ch);
    if (this.dataChannels.length === 1) this.#negotiationNeeded();
    return ch;
  }

  // ---- negotiation ----

  createOffer(): Promise<RTCSessionDescriptionInit> {
    return this.#run('createOffer', () => {
      const sdp = buildSdp({ ufrag: this.#ufrag, setup: 'actpass', sections: this.#localSections() });
      return { type: 'offer', sdp };
    });
  }

  createAnswer(): Promise<RTCSessionDescriptionInit> {
    return this.#run('createAnswer', () => {
      if (this.signalingState !== 'have-remote-offer' || !this.remoteDescription?.sdp) {
        throw invalidState('createAnswer needs a remote offer');
      }
      const sections = parseSdpSections(this.remoteDescription.sdp).map((s): SdpSection => {
        const t = this.transceivers.find((x) => x.mid === s.mid);
        let direction = FLIP[s.direction];
        // We send only when our transceiver wants to and has a track.
        if (
          (direction === 'sendonly' || direction === 'sendrecv') &&
          !(t?.sender.track && t.direction.startsWith('send'))
        ) {
          direction = direction === 'sendrecv' ? 'recvonly' : 'inactive';
        }
        return { ...s, direction, msid: undefined };
      });
      return { type: 'answer', sdp: buildSdp({ ufrag: this.#ufrag, setup: 'active', sections }) };
    });
  }

  setLocalDescription(desc?: RTCSessionDescriptionInit): Promise<void> {
    return this.#run('setLocalDescription', async () => {
      let d = desc;
      if (!d?.sdp) {
        const type = d?.type ?? (this.signalingState === 'have-remote-offer' ? 'answer' : 'offer');
        if (type === 'rollback') d = { type };
        else d = type === 'answer' ? await this.createAnswer() : await this.createOffer();
      }
      switch (d.type) {
        case 'offer':
          if (this.signalingState !== 'stable' && this.signalingState !== 'have-local-offer') {
            throw invalidState(`setLocalDescription(offer) in ${this.signalingState}`);
          }
          for (const t of this.transceivers) if (t.mid === null && !t.stopped) t.mid = String(this.#nextMid++);
          this.localDescription = { type: 'offer', sdp: d.sdp ?? '' };
          this.#setSignaling('have-local-offer');
          break;
        case 'answer':
        case 'pranswer':
          if (this.signalingState !== 'have-remote-offer') {
            throw invalidState(`setLocalDescription(answer) in ${this.signalingState}`);
          }
          this.localDescription = { type: d.type, sdp: d.sdp ?? '' };
          this.#applyCurrentDirections();
          this.#setSignaling('stable');
          break;
        case 'rollback':
          this.#setSignaling('stable');
          break;
      }
    });
  }

  setRemoteDescription(desc: RTCSessionDescriptionInit): Promise<void> {
    return this.#run('setRemoteDescription', () => {
      switch (desc.type) {
        case 'offer': {
          if (this.signalingState !== 'stable' && this.signalingState !== 'have-remote-offer') {
            throw invalidState(`setRemoteDescription(offer) in ${this.signalingState}`);
          }
          this.remoteDescription = { type: 'offer', sdp: desc.sdp ?? '' };
          const events = this.#applyRemoteSections(parseSdpSections(desc.sdp ?? ''), true);
          this.#setSignaling('have-remote-offer');
          for (const ev of events) this.#fire('track', ev);
          break;
        }
        case 'answer':
        case 'pranswer': {
          if (this.signalingState !== 'have-local-offer') {
            throw invalidState(`setRemoteDescription(answer) in ${this.signalingState}`);
          }
          this.remoteDescription = { type: desc.type, sdp: desc.sdp ?? '' };
          const events = this.#applyRemoteSections(parseSdpSections(desc.sdp ?? ''), false);
          this.#applyCurrentDirections();
          this.#setSignaling('stable');
          for (const ev of events) this.#fire('track', ev);
          break;
        }
        case 'rollback':
          this.#setSignaling('stable');
          break;
      }
    });
  }

  addIceCandidate(candidate?: RTCIceCandidateInit | null): Promise<void> {
    return this.#run('addIceCandidate', () => {
      if (!this.remoteDescription) throw invalidState('addIceCandidate before setRemoteDescription');
      if (candidate) this.remoteCandidates.push({ ...candidate });
    });
  }

  restartIce(): void {
    this.restartIceCalls++;
    this.#ufrag = nextToken('lf');
    this.#negotiationNeeded();
  }

  getStats(): Promise<Map<string, Record<string, unknown>>> {
    return Promise.resolve(new Map(this.stats));
  }

  close(): void {
    if (this.signalingState === 'closed') return;
    // Like browsers: no state-change events for a local close().
    this.signalingState = 'closed';
    this.connectionState = 'closed';
    this.iceConnectionState = 'closed';
    for (const t of this.transceivers) t.stop();
    for (const ch of this.dataChannels) ch.close();
  }

  // ---- test controls ----

  /** The network side: sets connectionState and fires connectionstatechange. */
  setConnectionState(state: RTCPeerConnectionState): void {
    this.connectionState = state;
    this.#fire('connectionstatechange', new Event('connectionstatechange'));
  }

  /** The network side: sets iceConnectionState and fires iceconnectionstatechange. */
  setIceConnectionState(state: RTCIceConnectionState): void {
    this.iceConnectionState = state;
    this.#fire('iceconnectionstatechange', new Event('iceconnectionstatechange'));
  }

  /** A local candidate was gathered (null: gathering finished). */
  emitIceCandidate(candidate: RTCIceCandidateInit | null): void {
    const ev = Object.assign(new Event('icecandidate'), { candidate });
    this.#fire('icecandidate', ev);
  }

  /** The other side opened a data channel. */
  emitDataChannel(label: string): FakeRTCDataChannel {
    const channel = new FakeRTCDataChannel(label);
    this.dataChannels.push(channel);
    this.#fire('datachannel', Object.assign(new Event('datachannel'), { channel }));
    return channel;
  }

  /** The next call of method rejects with error (then it works again). */
  failNext(method: FailableMethod, error: unknown = new DOMException('fake failure', 'OperationError')): void {
    this.#failures.set(method, error);
  }

  /** The current local ICE ufrag (it changes on restartIce). */
  get localUfrag(): string {
    return this.#ufrag;
  }

  /** The remote description's ICE ufrag. */
  get remoteUfrag(): string | null {
    return this.remoteDescription?.sdp ? ufragOf(this.remoteDescription.sdp) : null;
  }

  // ---- internals ----

  #assertOpen(): void {
    if (this.signalingState === 'closed') throw invalidState('the connection is closed');
  }

  async #run<T>(method: FailableMethod, fn: () => T | Promise<T>): Promise<T> {
    await Promise.resolve();
    if (this.signalingState === 'closed') throw invalidState(`${method} on a closed connection`);
    if (this.#failures.has(method)) {
      const err = this.#failures.get(method);
      this.#failures.delete(method);
      throw err;
    }
    return fn();
  }

  #localSections(): SdpSection[] {
    const sections: SdpSection[] = [];
    let provisional = this.#nextMid;
    for (const t of this.transceivers) {
      if (t.stopped) continue;
      const rids = t.sender.parameters.encodings.flatMap((e) => e.rid ?? []);
      sections.push({
        kind: t.kind,
        mid: t.mid ?? String(provisional++),
        direction: t.direction,
        ...(t.sender.track ? { msid: { stream: 'local', track: t.sender.track.id } } : {}),
        ...(rids.length > 1 ? { rids } : {}),
        ...(t.codecPreferences.length > 0 ? { codecLines: codecLinesOf(t.codecPreferences) } : {}),
      });
    }
    if (this.dataChannels.length > 0)
      sections.push({ kind: 'application', mid: String(provisional), direction: 'sendrecv' });
    return sections;
  }

  /** Applies remote m-sections to the transceivers; returns the track events to fire. */
  #applyRemoteSections(sections: SdpSection[], isOffer: boolean): Event[] {
    const events: Event[] = [];
    for (const s of sections) {
      if (s.kind === 'application') continue;
      let t = this.transceivers.find((x) => x.mid === s.mid);
      if (!t && isOffer) {
        t = new FakeRTCRtpTransceiver(s.kind, 'recvonly', null);
        t.mid = s.mid;
        this.transceivers.push(t);
      }
      if (!t) continue;
      const sending = s.direction === 'sendonly' || s.direction === 'sendrecv';
      const msid = s.msid ? `${s.msid.stream} ${s.msid.track}` : null;
      if (sending && (!t.remoteSending || msid !== t.remoteMsid)) {
        const streams = s.msid ? [new FakeMediaStream([t.receiver.track], s.msid.stream)] : [];
        events.push(
          Object.assign(new Event('track'), { track: t.receiver.track, receiver: t.receiver, transceiver: t, streams }),
        );
      }
      t.remoteSending = sending;
      t.remoteMsid = msid;
    }
    return events;
  }

  #applyCurrentDirections(): void {
    const local = this.localDescription?.sdp ? parseSdpSections(this.localDescription.sdp) : [];
    for (const s of local) {
      const t = this.transceivers.find((x) => x.mid === s.mid);
      if (t && !t.stopped) t.currentDirection = s.direction;
    }
  }

  #setSignaling(state: RTCSignalingState): void {
    if (this.signalingState === state) return;
    this.signalingState = state;
    this.#fire('signalingstatechange', new Event('signalingstatechange'));
  }

  #negotiationNeeded(): void {
    if (this.#negotiationQueued) return;
    this.#negotiationQueued = true;
    queueMicrotask(() => {
      this.#negotiationQueued = false;
      if (this.signalingState === 'stable') this.#fire('negotiationneeded', new Event('negotiationneeded'));
    });
  }

  #fire(type: string, ev: Event): void {
    const handler = (this as unknown as Record<string, Handler>)[`on${type}`];
    handler?.call(this, ev);
    this.dispatchEvent(ev);
  }
}

/** SDP codec lines for setCodecPreferences' codecs, payload types from 96 up. */
function codecLinesOf(codecs: readonly RTCRtpCodec[]): string[] {
  const lines: string[] = [];
  let pt = 96;
  let lastMedia = 0;
  for (const c of codecs) {
    const [, name = ''] = c.mimeType.split('/');
    const rate = String(c.clockRate);
    lines.push(
      `a=rtpmap:${String(pt)} ${name}/${rate}${c.channels !== undefined && c.channels > 1 ? `/${String(c.channels)}` : ''}`,
    );
    if (name.toLowerCase() === 'rtx') lines.push(`a=fmtp:${String(pt)} apt=${String(lastMedia)}`);
    else {
      lastMedia = pt;
      if (c.sdpFmtpLine) lines.push(`a=fmtp:${String(pt)} ${c.sdpFmtpLine}`);
    }
    pt++;
  }
  return lines;
}

/** Codec capabilities like a desktop Chrome's: H.264 High and Constrained Baseline (packetization-mode 1), VP8, rtx, Opus. */
export const FAKE_VIDEO_CODECS: readonly RTCRtpCodec[] = [
  { mimeType: 'video/VP8', clockRate: 90000 },
  { mimeType: 'video/rtx', clockRate: 90000 },
  {
    mimeType: 'video/H264',
    clockRate: 90000,
    sdpFmtpLine: 'level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=640034',
  },
  {
    mimeType: 'video/H264',
    clockRate: 90000,
    sdpFmtpLine: 'level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f',
  },
  {
    mimeType: 'video/H264',
    clockRate: 90000,
    sdpFmtpLine: 'level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=42e01f',
  },
];

export const FAKE_AUDIO_CODECS: readonly RTCRtpCodec[] = [
  { mimeType: 'audio/opus', clockRate: 48000, channels: 2, sdpFmtpLine: 'minptime=10;useinbandfec=1' },
];

export interface InstallFakeRTCOptions {
  /** Whether the codec capabilities include H.264 (default true). */
  h264?: boolean;
  /** Whether navigator.mediaDevices.getDisplayMedia exists (default false). */
  displayMedia?: boolean;
}

/**
 * Stubs the WebRTC globals: RTCPeerConnection (this fake), RTCRtpTransceiver, and getCapabilities on RTCRtpSender and
 * RTCRtpReceiver, so 01's detectCaps() sees a browser; with displayMedia, also navigator.mediaDevices with a
 * getDisplayMedia mock. Returns the function that undoes it all (it calls vi.unstubAllGlobals()).
 */
export function installFakeRTC({ h264 = true, displayMedia = false }: InstallFakeRTCOptions = {}): () => void {
  const video = h264 ? FAKE_VIDEO_CODECS : FAKE_VIDEO_CODECS.filter((c) => c.mimeType !== 'video/H264');
  const caps = (kind: string): RTCRtpCapabilities | null =>
    kind === 'video'
      ? { codecs: video.map((c) => ({ ...c })), headerExtensions: [] }
      : kind === 'audio'
        ? { codecs: FAKE_AUDIO_CODECS.map((c) => ({ ...c })), headerExtensions: [] }
        : null;
  vi.stubGlobal('RTCPeerConnection', FakeRTCPeerConnection);
  vi.stubGlobal('RTCRtpTransceiver', FakeRTCRtpTransceiver);
  vi.stubGlobal('RTCRtpSender', { getCapabilities: caps });
  vi.stubGlobal('RTCRtpReceiver', { getCapabilities: caps });
  // navigator is jsdom's object with its own getters: add the property to it instead of replacing it.
  const had = Object.getOwnPropertyDescriptor(globalThis.navigator, 'mediaDevices');
  if (displayMedia) {
    Object.defineProperty(globalThis.navigator, 'mediaDevices', {
      configurable: true,
      value: { getDisplayMedia: vi.fn(), getUserMedia: vi.fn() },
    });
  }
  return () => {
    vi.unstubAllGlobals();
    if (!displayMedia) return;
    if (had) Object.defineProperty(globalThis.navigator, 'mediaDevices', had);
    else Reflect.deleteProperty(globalThis.navigator, 'mediaDevices');
  };
}
