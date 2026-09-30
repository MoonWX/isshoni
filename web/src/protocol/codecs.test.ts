// codecs.ts (01 §19): h264Key on the fmtp table of Go's ParseH264CodecKey test, and detectCaps against stubbed
// getCapabilities results.
import { afterEach, describe, expect, it, vi } from 'vitest';

import { detectCaps, h264Key } from './codecs';
import * as types from './types.gen';

const {
  CodecH264Baseline: baseline,
  CodecH264ConstrainedBaseline: constrainedBaseline,
  CodecH264ConstrainedHigh: constrainedHigh,
  CodecH264High: high,
  CodecH264Main: main,
} = types;

/**
 * TestParseH264CodecKey's table in internal/protocol/codec_test.go, row for row. "same as Go" below fails when that
 * table has a row this one lacks.
 */
const goRows: [fmtp: string, want: string][] = [
  ['level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f', constrainedBaseline],
  ['level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f', baseline],
  ['level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=4d001f', main],
  ['level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=640c1f', constrainedHigh],
  ['level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=64001f', high],
  ['level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=640034', high], // Chrome's High: level ignored
  ['profile-level-id=42e01f;packetization-mode=1', constrainedBaseline], // order does not matter
  ['packetization-mode=1; profile-level-id=42e01f', constrainedBaseline], // spaces
  ['a=fmtp:102 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=64001f', high],

  // mode 0 (or no mode, which means 0): not usable.
  ['level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=42e01f', ''],
  ['level-asymmetry-allowed=1;profile-level-id=42e01f', ''],
  ['packetization-mode=2;profile-level-id=42e01f', ''],

  // mixed case: names and hex digits.
  ['Packetization-Mode=1;Profile-Level-Id=42E01F', constrainedBaseline],
  ['packetization-mode=1;profile-level-id=640C1F', constrainedHigh],
  ['PACKETIZATION-MODE=1;PROFILE-LEVEL-ID=4D001F', main],

  // no or malformed profile-level-id.
  ['packetization-mode=1', ''],
  ['packetization-mode=1;profile-level-id=', ''],
  ['packetization-mode=1;profile-level-id=42e0', ''],
  ['packetization-mode=1;profile-level-id=42e01f00', ''],
  ['packetization-mode=1;profile-level-id=zze01f', ''],
  ['', ''],
  ['minptime=10;useinbandfec=1', ''], // Opus
];

/** Behavior of the Go implementation that its table does not spell out. */
const extraRows: [fmtp: string, want: string][] = [
  ['packetization-mode = 1 ; profile-level-id = 42e01f ', constrainedBaseline],
  ['packetization-mode=1;profile-level-id=42e01f;packetization-mode=0', ''], // the last duplicate wins
  ['packetization-mode=0;profile-level-id=42e01f;packetization-mode=1', constrainedBaseline],
  ['a=fmtp:102', ''], // a prefix without parameters
  ['packetization-mode;profile-level-id=42e01f', ''],
  ['packetization-mode=01;profile-level-id=42e01f', ''],
  ['packetization-mode=1;profile-level-id=+42e01', ''],
];

/** The Node built-ins readRepoFile needs, typed here: the app's tsconfig has no Node types. */
interface NodeBuiltins {
  'node:fs': { readFileSync: (path: string, encoding: 'utf8') => string };
  'node:path': { resolve: (...paths: string[]) => string; dirname: (path: string) => string };
  'node:url': { fileURLToPath: (url: string) => string };
}

function builtin<K extends keyof NodeBuiltins>(id: K): NodeBuiltins[K] {
  const proc = (globalThis as { process?: { getBuiltinModule?: (id: string) => unknown } }).process;
  const m = proc?.getBuiltinModule?.(id);
  if (m === undefined) {
    throw new Error(`${id} is not available: this test runs in Node`);
  }
  return m as NodeBuiltins[K];
}

/**
 * Reads a repository file, relative to this one, through Node's fs. Vitest runs in Node, but Vite refuses ?raw
 * imports from outside web/.
 */
function readRepoFile(fromHere: string): string {
  const path = builtin('node:path');
  const here = path.dirname(builtin('node:url').fileURLToPath(import.meta.url));
  return builtin('node:fs').readFileSync(path.resolve(here, fromHere), 'utf8');
}

/**
 * The rows of Go's TestParseH264CodecKey, {"<fmtp>", <CodecH264… constant or "">}. The fmtp is a Go string literal
 * that JSON.parse reads; each constant resolves through types.gen.ts, which tygo generates from the same Go consts.
 */
function parseGoTable(src: string): [string, string][] {
  const start = src.indexOf('func TestParseH264CodecKey(');
  if (start < 0) {
    throw new Error('TestParseH264CodecKey not found');
  }
  const end = src.indexOf('\nfunc ', start + 1);
  const consts = types as unknown as Record<string, unknown>;
  const rows: [string, string][] = [];
  for (const [, fmtp = '', want = ''] of src
    .slice(start, end < 0 ? undefined : end)
    .matchAll(/\{("(?:[^"\\]|\\.)*"),\s*(\w+|"")\}/g)) {
    const value = want === '""' ? '' : consts[want];
    if (typeof value !== 'string') {
      throw new Error(`${want} is not a string constant of types.gen.ts`);
    }
    rows.push([JSON.parse(fmtp) as string, value]);
  }
  return rows;
}

describe('h264Key', () => {
  it.each([...goRows, ...extraRows])('%j → %j', (fmtp, want) => {
    expect(h264Key(fmtp)).toBe(want);
  });

  it('covers 01 §19: 42e01f, 42001f, 4d001f, 640c1f, 64001f, 640034, mode 0, mixed case', () => {
    const covered = goRows.map(([fmtp]) => fmtp).join('\n');
    for (const needle of [
      '42e01f',
      '42001f',
      '4d001f',
      '640c1f',
      '64001f',
      '640034',
      'packetization-mode=0',
      '4D001F',
    ]) {
      expect(covered).toContain(needle);
    }
  });

  it('is tested on the same table as Go (internal/protocol/codec_test.go)', () => {
    const go = parseGoTable(readRepoFile('../../../internal/protocol/codec_test.go'));
    expect(go.length).toBeGreaterThanOrEqual(goRows.length);
    // A Go row missing here means Go's behavior changed or grew: copy the row into goRows.
    expect(goRows).toEqual(go);
  });
});

function codec(mimeType: string, sdpFmtpLine?: string): RTCRtpCodec {
  const c: RTCRtpCodec = { mimeType, clockRate: mimeType.startsWith('audio/') ? 48000 : 90000 };
  if (sdpFmtpLine !== undefined) {
    c.sdpFmtpLine = sdpFmtpLine;
  }
  return c;
}

const h264 = (profile: string, mode = 1) =>
  codec('video/H264', `level-asymmetry-allowed=1;packetization-mode=${String(mode)};profile-level-id=${profile}`);

// Roughly what Chrome on macOS reports (S4): both packetization modes of each profile, VP8/VP9/AV1, rtx and FEC.
const chromeVideo = [
  codec('video/VP8'),
  codec('video/rtx', 'apt=96'),
  h264('42001f'),
  h264('42001f', 0),
  h264('42e01f'),
  h264('42e01f', 0),
  h264('4d001f'),
  h264('4d001f', 0),
  h264('f4001f'),
  codec('video/VP9', 'profile-id=0'),
  h264('64001f'),
  h264('640034'), // the same key as 64001f: listed once
  codec('video/AV1', 'level-idx=5;profile=0;tier=0'),
  codec('video/red'),
  codec('video/ulpfec'),
];
const chromeAudio = [
  codec('audio/opus', 'minptime=10;useinbandfec=1'),
  codec('audio/red', '111/111'),
  codec('audio/G722'),
];

type Capabilities = Partial<Record<'audio' | 'video', RTCRtpCodec[] | null>>;

function stubSide(name: 'RTCRtpReceiver' | 'RTCRtpSender', caps: Capabilities): void {
  vi.stubGlobal(name, {
    getCapabilities: (kind: string): RTCRtpCapabilities | null => {
      const codecs = caps[kind as 'audio' | 'video'];
      return codecs ? { codecs, headerExtensions: [] } : null;
    },
  });
}

function stubSharing(): void {
  vi.stubGlobal('RTCRtpTransceiver', vi.fn()); // a function is all detectCaps looks for
  vi.stubGlobal('navigator', { mediaDevices: { getDisplayMedia: vi.fn() } });
}

describe('detectCaps', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('lists the H.264 packetization-mode=1 profiles once each, in browser order, then opus', () => {
    stubSide('RTCRtpReceiver', { video: chromeVideo, audio: chromeAudio });
    stubSide('RTCRtpSender', { video: chromeVideo, audio: chromeAudio });
    stubSharing();
    const want = ['h264/4200', 'h264/42e0', 'h264/4d00', 'h264/f400', 'h264/6400', 'opus'];
    expect(detectCaps()).toStrictEqual({ decode: want, encode: want, simulcast: true, displayCapture: true });
  });

  it('reports audio only for a fresh Firefox profile (no OpenH264 yet, 01 §11.7)', () => {
    const video = [codec('video/VP8'), codec('video/VP9')];
    const audio = [codec('audio/opus', 'maxplaybackrate=48000;stereo=1;useinbandfec=1')];
    stubSide('RTCRtpReceiver', { video, audio });
    stubSide('RTCRtpSender', { video, audio });
    stubSharing();
    // No H.264 encoder: neither simulcast nor display capture, even though getDisplayMedia exists.
    expect(detectCaps()).toStrictEqual({ decode: ['opus'], encode: ['opus'] });
  });

  it('matches MIME types case-insensitively and skips entries without an fmtp line', () => {
    stubSide('RTCRtpReceiver', {
      video: [codec('video/h264', 'packetization-mode=1;profile-level-id=640c1f'), codec('video/H264')],
      audio: [codec('AUDIO/OPUS')],
    });
    expect(detectCaps()).toStrictEqual({ decode: ['h264/640c', 'opus'] });
  });

  it('omits encode, simulcast and displayCapture when the browser cannot send', () => {
    stubSide('RTCRtpReceiver', { video: [h264('42e01f')], audio: chromeAudio });
    stubSide('RTCRtpSender', { video: [h264('42e01f', 0)], audio: null });
    stubSharing();
    expect(detectCaps()).toStrictEqual({ decode: ['h264/42e0', 'opus'] });
  });

  it('needs RTCRtpTransceiver for simulcast and getDisplayMedia for display capture', () => {
    stubSide('RTCRtpSender', { video: [h264('64001f')] });
    expect(detectCaps()).toStrictEqual({ decode: [], encode: ['h264/6400'] });

    vi.stubGlobal('navigator', { mediaDevices: { getDisplayMedia: vi.fn() } });
    expect(detectCaps()).toStrictEqual({ decode: [], encode: ['h264/6400'], displayCapture: true });

    vi.stubGlobal('RTCRtpTransceiver', vi.fn());
    expect(detectCaps()).toStrictEqual({ decode: [], encode: ['h264/6400'], simulcast: true, displayCapture: true });
  });

  it('gives empty lists without WebRTC, and when getCapabilities returns null or throws', () => {
    vi.stubGlobal('RTCRtpReceiver', undefined);
    vi.stubGlobal('RTCRtpSender', undefined);
    expect(detectCaps()).toStrictEqual({ decode: [] });

    stubSide('RTCRtpReceiver', { video: null, audio: null });
    vi.stubGlobal('RTCRtpSender', {
      getCapabilities: () => {
        throw new TypeError('unsupported kind');
      },
    });
    expect(detectCaps()).toStrictEqual({ decode: [] });

    vi.stubGlobal('RTCRtpReceiver', {}); // no static getCapabilities
    expect(detectCaps()).toStrictEqual({ decode: [] });
  });

  it(`never lists more than MaxCodecs (${String(types.MaxCodecs)}) keys, which the server would reject`, () => {
    const many = Array.from({ length: 50 }, (_, i) => h264(`64${i.toString(16).padStart(2, '0')}1f`));
    stubSide('RTCRtpReceiver', { video: many, audio: chromeAudio });
    const { decode } = detectCaps();
    expect(decode).toHaveLength(types.MaxCodecs);
    expect(new Set(decode).size).toBe(types.MaxCodecs);
    expect(decode[0]).toBe('h264/6400');
  });
});
