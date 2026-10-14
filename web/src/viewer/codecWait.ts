// The codec wait (05 §10.6; 01 §8.10, §11.7; 02 §8.5): what the page does while the server holds a share's video
// back because this browser can't decode it yet. The server says so with subscribe.status {video: off, reason:
// codec}; the audio keeps playing, and the tile says why there is no video (shareView.ts). A fresh Firefox profile
// is the usual case: it downloads Cisco's OpenH264 plugin in the background, about a minute, before it has H.264.
//
// - **The page's whole part in codec recovery is caps.update.** While any subscription has the reason `codec`, it
//   reads the browser's capabilities again every 5 s (01's detectCaps()), and when the H.264 decoders differ from
//   the ones it last told the server about, it sends caps.update {caps}. The server then rebuilds the sub PC with
//   the video (gen + 1, 02 §8.5), and the SubscriberPC answers that offer like any other.
// - **Never pc.restart for codecs** (01 §11.7, 05 §22 C2). A Firefox that lists H.264 before it can decode it gets
//   the server's own sub PC rebuilds, every 20 s, at most 9 times; the page only answers them.
// - **What the server knows** is the caps of the page's first hello, then of every caps.update that went out. A
//   hello after a reconnect reads the caps afresh, so after one the page may send a caps.update the server already
//   has: equal caps start nothing there (02 §8.5), so that costs one message and is never wrong. caps.update goes
//   out at most once per 5 s, inside 01's 12 per minute.
// - **After 3 minutes** (02's 9 retries of 20 s) the page asks for a reload, which brings fresh caps and a fresh PC
//   (README §6 "Decided after group 9" (4): after `decoder_failed` only a caps change or a reload starts the
//   server's retries again). Firefox gets the text about its OpenH264 plugin.
// - **A browser that is not Firefox and lists no H.264 decoder at all** won't get one by waiting: its page says it
//   can't play isshoni video (05 §10.6), and the audio still plays.
//
// The wait is the page's, not a share's: it begins with the first subscription that has the reason `codec` and
// ends when none has it. A gap shorter than 30 s (one share ending as another starts, a status the server's rebuild
// changes for a moment) doesn't start the 3 minutes again. No React here (05 §3): the components read
// viewerStore.codecWait through codecNotice().
import { createLogger, type Logger } from '../lib/log';
import { browserName, type UAData } from '../lib/ua';
import { detectCaps } from '../protocol/codecs';
import type { SignalClient } from '../protocol/signal-client';
import { MessageTypeCapsUpdate, StatusReasonCodec, type Caps, type CodecKey } from '../protocol/types.gen';
import type { CodecWait, ViewerState, ViewerStore } from './viewerStore';

/** The capabilities are read again this often while a subscription waits for a decoder (05 §18). */
export const CAPS_POLL_MS = 5_000;
/** After this long without a decoder the page asks for a reload (05 §10.6, §18; 02 §8.5's 20 s × 9). */
export const CODEC_STUCK_MS = 180_000;
/** A wait that comes back within this long after it ended goes on from where it was: the 3 minutes don't restart. */
export const CODEC_WAIT_GAP_MS = 30_000;
/** At most one caps.update per this long (01 §13: 12 per minute). */
export const CAPS_UPDATE_SPACING_MS = 5_000;

/**
 * What the page says about the codec wait:
 * - `waiting`: "Your browser is still getting its video decoder … Retrying…" on the tile;
 * - `stuck`, `stuckFirefox`: 3 minutes without a decoder: "Reload the page", with Firefox's own help;
 * - `missing`: a browser other than Firefox without any H.264 decoder: "This browser can't play isshoni video".
 */
export type CodecNotice = 'waiting' | 'stuck' | 'stuckFirefox' | 'missing';

/** The notice for viewerStore.codecWait; null while there is no wait. Pure, so a selector can use it. */
export function codecNotice(wait: CodecWait | null): CodecNotice | null {
  if (wait === null) return null;
  if (wait.noDecoder && !wait.firefox) return 'missing';
  if (wait.stuck) return wait.firefox ? 'stuckFirefox' : 'stuck';
  return 'waiting';
}

/** What the wait needs from signaling: 01's SignalClient sends caps.update. */
export type CodecWaitSignal = Pick<SignalClient, 'notify'>;

export interface CodecWaitOptions {
  /** The browser's capabilities now; default 01's detectCaps() (what platform.capsNow() reads in a browser). */
  caps?: () => Caps;
  /** Whether the browser is Firefox; default: from the user agent (lib/ua.ts, which picks help text only). */
  firefox?: boolean;
  /** Default Date.now. */
  now?: () => number;
  log?: Logger;
}

/** The H.264 decoders of a caps value, sorted: what the server's codec policy compares (02 §8.5: as sets). */
function h264Decoders(caps: Caps): CodecKey[] {
  return [...new Set(caps.decode.filter((key) => key.startsWith('h264/')))].sort();
}

function sameKeys(a: readonly CodecKey[], b: readonly CodecKey[]): boolean {
  return a.length === b.length && a.every((key, i) => key === b[i]);
}

/** Whether some subscription's video is held back for the codec (01 §8.9: reason `codec`). */
function waitsForCodec(s: ViewerState): boolean {
  return Object.values(s.status).some((status) => status.reason === StatusReasonCodec);
}

/** Firefox, by its user agent; false where there is no navigator. */
function uaIsFirefox(): boolean {
  const nav = (globalThis as { navigator?: { userAgent?: string; userAgentData?: UAData } }).navigator;
  return browserName(nav?.userAgent ?? '', nav?.userAgentData) === 'firefox';
}

/**
 * Runs the codec wait for the page's viewer store: viewerStore.codecWait, the 5 s caps poll while some subscription
 * has the reason `codec`, and caps.update when the H.264 decoders changed. attachViewer (services.ts) starts it
 * with the room's SignalClient. Returns the function that stops it.
 */
export function attachCodecWait(signal: CodecWaitSignal, store: ViewerStore, opts: CodecWaitOptions = {}): () => void {
  const readCaps = opts.caps ?? detectCaps;
  const now = opts.now ?? Date.now;
  const firefox = opts.firefox ?? uaIsFirefox();
  const log = opts.log ?? createLogger('viewer').child('codec');

  const read = (): Caps | null => {
    try {
      return readCaps();
    } catch (err) {
      log.warn('the capabilities could not be read', { err });
      return null;
    }
  };

  const first = read();
  /** The H.264 decoders the server was last told about: the hello's, then each caps.update's. */
  let told: CodecKey[] = first === null ? [] : h264Decoders(first);
  let noDecoder = first !== null && h264Decoders(first).length === 0;
  let lastSent = Number.NEGATIVE_INFINITY;

  let waiting = false;
  /** When the wait began; kept for CODEC_WAIT_GAP_MS after it ended. */
  let since: number | null = null;
  let endedAt: number | null = null;
  let stuck = false;
  let poll: ReturnType<typeof setInterval> | undefined;
  let stuckTimer: ReturnType<typeof setTimeout> | undefined;

  const publish = (): void => {
    store.getState().setCodecWait(waiting && since !== null ? { since, stuck, noDecoder, firefox } : null);
  };

  /** One read of the capabilities: caps.update when the decoders changed. */
  const check = (): void => {
    const caps = read();
    if (caps !== null) {
      const decoders = h264Decoders(caps);
      noDecoder = decoders.length === 0;
      const at = now();
      if (!sameKeys(decoders, told) && at - lastSent >= CAPS_UPDATE_SPACING_MS) {
        // false while signaling isn't ready: the next poll tries again (or the next hello carries the caps).
        if (signal.notify(MessageTypeCapsUpdate, { caps })) {
          log.info('the video decoders changed: caps.update sent', { h264: decoders.length });
          told = decoders;
          lastSent = at;
        }
      }
    }
    publish();
  };

  const stopTimers = (): void => {
    clearInterval(poll);
    clearTimeout(stuckTimer);
    poll = undefined;
    stuckTimer = undefined;
  };

  const follow = (s: ViewerState): void => {
    const want = waitsForCodec(s);
    if (want === waiting) return;
    waiting = want;
    const at = now();
    if (!want) {
      endedAt = at;
      stopTimers();
      log.info('the codec wait is over');
      publish();
      return;
    }
    if (since === null || endedAt === null || at - endedAt > CODEC_WAIT_GAP_MS) {
      since = at;
      log.info('waiting for a video decoder');
    }
    endedAt = null;
    const left = since + CODEC_STUCK_MS - at;
    stuck = left <= 0;
    if (!stuck) {
      stuckTimer = setTimeout(() => {
        stuck = true;
        log.warn('no video decoder after 3 minutes: the page asks for a reload');
        publish();
      }, left);
    }
    poll = setInterval(check, CAPS_POLL_MS);
    // At once too: the decoder may have come between the hello and this status.
    check();
  };

  const off = store.subscribe(follow);
  follow(store.getState());
  return () => {
    off();
    stopTimers();
    waiting = false;
    publish();
  };
}
