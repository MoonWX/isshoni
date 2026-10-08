// Test support for the publishing half of the sharer: ShareParams as the server sends them, the server's answer to
// a pub offer, and the hub's side of a share (share.start, share.update, share.stop, room.state).
import { FakeSignalServer } from '../../protocol/testing';
import type { PCAnswer, PCOffer, RoomState, ShareInfo, ShareParams } from '../../protocol/types.gen';
import { buildSdp, FakeRTCPeerConnection, parseSdpSections } from '../../test/FakeRTCPeerConnection';

/** share.start's answer for the Auto preset (02 §8.6), in wire order: high first. */
export function shareParams(shareId = 's_a', overrides: Partial<ShareParams> = {}): ShareParams {
  return {
    shareId,
    codec: 'h264/6400',
    encodings: [
      { rid: 'f', layer: 'high', active: true, maxBitrate: 8_000_000, maxFramerate: 60, maxPixels: 2_073_600 },
      { rid: 'q', layer: 'low', active: true, maxBitrate: 300_000, maxFramerate: 15, maxPixels: 230_400 },
    ],
    audioBitrate: 128_000,
    ...overrides,
  };
}

/** The SFU's answer to a pub offer: every sending m-section received, the rest inactive. */
export function answerFor(offer: PCOffer, ufrag = 'srv1'): PCAnswer {
  const sections = parseSdpSections(offer.sdp).map((s) => ({
    kind: s.kind,
    mid: s.mid,
    direction: s.direction === 'sendonly' || s.direction === 'sendrecv' ? ('recvonly' as const) : ('inactive' as const),
    ...(s.rids ? { rids: s.rids } : {}),
    ...(s.codecLines ? { codecLines: s.codecLines } : {}),
  }));
  return { pc: 'pub', gen: offer.gen, neg: offer.neg, sdp: buildSdp({ ufrag, setup: 'active', sections }) };
}

/** The newest fake RTCPeerConnection; throws when none was made. */
export function lastPc(): FakeRTCPeerConnection {
  const pc = FakeRTCPeerConnection.last;
  if (!pc) throw new Error('no RTCPeerConnection was created');
  return pc;
}

/** The pc.offer {pc: 'pub'} messages the client sent, oldest first. */
export function pubOffers(server: FakeSignalServer): PCOffer[] {
  return server.messages('pc.offer').map((m) => m.data as PCOffer);
}

export function shareInfo(id: string, overrides: Partial<ShareInfo> = {}): ShareInfo {
  return {
    id,
    userId: 'k3m9p2qxw7ht',
    connectionId: 'c_me',
    kind: 'window',
    preset: 'auto',
    audio: true,
    status: 'live',
    layers: ['high', 'low'],
    codec: 'h264/6400',
    startedAt: '2026-10-12T19:02:30.000Z',
    watchers: [],
    ...overrides,
  };
}

/**
 * The hub's side of sharing on a fake signaling server: share.start answers with ShareParams (ids s_1, s_2, …),
 * share.update with the same params and the new preset's audio bitrate (Movie: 256 kbit/s), share.stop with ok.
 */
export class FakeShareHub {
  readonly #server: FakeSignalServer;
  #made = 0;
  #rev = 1;
  /** What the next share.start answers; default: shareParams('s_<n>'). */
  nextParams: Partial<ShareParams> = {};

  constructor(server: FakeSignalServer) {
    this.#server = server;
    server.handle('share.start', () => ({
      ok: shareParams(`s_${String(++this.#made)}`, this.nextParams),
    }));
    server.handle('share.update', (data) => ({
      ok: shareParams(data.shareId, { ...this.nextParams, audioBitrate: data.preset === 'movie' ? 256_000 : 128_000 }),
    }));
    server.handle('share.stop', () => ({ ok: {} }));
  }

  /** The data of the client's requests of one type, oldest first. */
  requests<T>(type: string): T[] {
    return this.#server.messages(type).map((m) => m.data as T);
  }

  /** Sends a room.state of roomId with these shares. */
  roomState(roomId: string, shares: ShareInfo[], overrides: Partial<RoomState> = {}): void {
    this.#server.send('room.state', { roomId, rev: this.#rev++, participants: [], shares, ...overrides });
  }

  /** Answers the newest pub offer like the SFU. */
  answer(): void {
    const offer = pubOffers(this.#server).at(-1);
    if (!offer) throw new Error('the client sent no pub offer');
    this.#server.send('pc.answer', answerFor(offer));
  }
}
