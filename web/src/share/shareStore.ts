// shareStore (05 §6.1, §13.1): the state machine of this page's own share. A vanilla Zustand store, so the
// controllers that don't import React (BrowserSharing, the room session) write it too; components read it with
// zustand's useStore.
//
//   idle ─Share click─► picking ──cancelled──► idle
//                         │ picked
//                         ▼
//                     confirming (only for a whole screen with system audio) ──pick again / cancel──► idle
//                         │ continue, with or without sound
//                         ▼
//                     starting ──► live ⇄ reconnecting ──► stopping ──► idle
//                         │ error
//                         ▼
//                      failed ──dismiss──► idle
//
// This file drives the first half: idle, picking, confirming, and the hand-over to `start` (the room session's
// startShare, which publishes through platform.sharing.start). The second half belongs to the publisher (S46):
// it moves `starting` to `live`, `reconnecting`, `stopping`, `idle` or `failed`, and fills `params` and `hint`,
// with setState on this store. M1 has one local share per page (05 §13.1), so one store per page: `shareStore`.
//
// Media stays out of the state (05 §6.1): the picked source lives in the store's closure from the pick until
// `start` takes it, and the state holds only its classification.
//
// The flow belongs to the page, not to the Share button that was clicked. The room page has two (05 §11.2), and
// the one in the empty state goes away as soon as a friend starts sharing, possibly while this user's picker or
// warning is open. So every mounted button attaches as a host: one of them (`hostId`) shows the warning, and a pick
// is given up only when the last one is gone.
import { createStore, type StoreApi } from 'zustand/vanilla';

import type { PickedSource, ShareHint } from '../platform/types';
import { PresetAuto, type Preset, type ShareParams } from '../protocol/types.gen';

export type SharePhase =
  'idle' | 'picking' | 'confirming' | 'starting' | 'live' | 'reconnecting' | 'stopping' | 'failed';

/** What was picked (05 §13.3), without the stream. */
export type PickedInfo = Pick<PickedSource, 'kind' | 'audioScope' | 'warning'>;

/**
 * Publishes a picked source: the room session's startShare (05 §11.1). From the call on, the source is the
 * callee's; when it rejects, the store releases the source and goes to `failed`.
 */
export type StartShare = (src: PickedSource, opts: { preset: Preset; withAudio: boolean }) => Promise<unknown>;

/** How one step of the flow ended, for the component that asked for it (toasts; the state has the rest). */
export type ShareFlowOutcome =
  /** Back to idle: the picker was cancelled, or the flow was cancelled or overtaken meanwhile. */
  | { readonly step: 'cancelled' }
  /** A whole screen with system audio: the ScreenAudioWarning decides (confirm, pick again or cancel). */
  | { readonly step: 'confirming' }
  /** `start` took the source and resolved. */
  | { readonly step: 'started'; readonly picked: PickedInfo; readonly withAudio: boolean }
  /** The capture failed (phase idle), or `start` rejected (phase failed). */
  | { readonly step: 'failed'; readonly error: unknown };

export interface ShareFields {
  readonly phase: SharePhase;
  /** The preset of the current pick or share (05 §13.5). */
  readonly preset: Preset;
  /** Whether sound is shared: the pick came with an audio track and the user kept it. */
  readonly withAudio: boolean;
  /** The classification of the picked source; null while idle or picking. */
  readonly picked: PickedInfo | null;
  /** The latest ShareParams (share.start, share.update, quality.hint). Filled by the publisher (S46). */
  readonly params: ShareParams | null;
  /** The upload or CPU hint of 05 §13.7. Filled by the publisher. */
  readonly hint: ShareHint | null;
  /** Why the phase is `failed`; null otherwise. */
  readonly error: unknown;
  /**
   * The attached host that shows the ScreenAudioWarning: the one attached longest; null while none is attached.
   * Whichever button's click started the pick, exactly one mounted button shows its warning.
   */
  readonly hostId: string | null;
}

export interface ShareActions {
  /**
   * Follows a pick that the click handler already started (`platform.sharing.pick()` must be the handler's first
   * statement, so its promise comes in here). idle, failed → picking; from confirming it is "Pick something else":
   * the current source is released first. Then: cancelled → idle; a whole screen with system audio → confirming;
   * anything else → starting, through opts.start.
   *
   * While a share is starting or live the pick isn't wanted: its source is released when it arrives.
   */
  pick(picking: Promise<PickedSource | null>, opts: { preset: Preset; start: StartShare }): Promise<ShareFlowOutcome>;
  /**
   * confirming → starting. Without sound ("Share without sound"), the audio track is stopped and taken out of the
   * stream first, at this click.
   */
  confirm(withAudio: boolean): Promise<ShareFlowOutcome>;
  /**
   * picking, confirming → idle; releases the picked source. A pick that resolves later is released too. The store
   * does this itself when the capture ends while the warning shows (the browser's "Stop sharing" bar).
   */
  cancel(): void;
  /** failed → idle. */
  dismiss(): void;
  /**
   * A Share button mounts: registers it as a host under an id of its own (React's useId) and returns its detach,
   * for the unmount. Detaching leaves the flow alone while another host is attached; the next in line becomes
   * `hostId`. When the last one detaches (the page is left), a pick that hasn't started is cancelled: the capture
   * must not outlive the UI that explains it. A share that is starting or live is left alone, as cancel() does.
   */
  attach(hostId: string): () => void;
}

export type ShareState = ShareFields & ShareActions;
export type ShareStore = StoreApi<ShareState>;

/** Everything but the preset, which outlives a share, and the host, which is about the page and not the share. */
const IDLE: Omit<ShareFields, 'preset' | 'hostId'> = Object.freeze({
  phase: 'idle',
  withAudio: false,
  picked: null,
  params: null,
  hint: null,
  error: null,
});

function infoOf(src: PickedSource): PickedInfo {
  return { kind: src.kind, audioScope: src.audioScope, warning: src.warning };
}

interface Waiting {
  readonly src: PickedSource;
  readonly start: StartShare;
  readonly unwatch: () => void;
}

export function createShareStore(): ShareStore {
  /**
   * While the warning shows: the picked source, what will publish it, and how to stop watching its video track.
   * Not state: it holds a MediaStream.
   */
  let waiting: Waiting | null = null;
  /** Bumped whenever the flow moves on, so a pick or a start that settles late knows it was overtaken. */
  let flow = 0;
  /** The attached hosts, oldest first. One entry per attach(), so a detach removes exactly its own. */
  const hosts: { readonly id: string }[] = [];

  return createStore<ShareState>()((set, get) => {
    /** Takes the waiting source out of the store; the caller starts it or releases it. */
    const takeWaiting = (): Waiting | null => {
      const taken = waiting;
      waiting = null;
      taken?.unwatch();
      return taken;
    };

    /**
     * The capture can end under the warning: the browser's own "Stop sharing" bar is there from the moment of the
     * pick. Nothing is left to confirm then, so the flow is cancelled. (A track's stop() fires no `ended`; only its
     * source ending does.)
     */
    const watchEnded = (src: PickedSource): (() => void) => {
      const tracks = src.preview.getVideoTracks();
      const onEnded = (): void => {
        if (waiting?.src === src) get().cancel();
      };
      for (const t of tracks) t.addEventListener('ended', onEnded);
      return () => {
        for (const t of tracks) t.removeEventListener('ended', onEnded);
      };
    };

    /** → starting: hands the source to start. */
    const begin = async (src: PickedSource, start: StartShare, withAudio: boolean): Promise<ShareFlowOutcome> => {
      const mine = ++flow;
      const picked = infoOf(src);
      set({ phase: 'starting', picked, withAudio, error: null });
      try {
        await start(src, { preset: get().preset, withAudio });
      } catch (error) {
        src.release();
        // Unless something else (the publisher, a newer pick) has moved the machine on meanwhile.
        if (flow === mine && get().phase === 'starting') set({ phase: 'failed', error });
        return { step: 'failed', error };
      }
      return { step: 'started', picked, withAudio };
    };

    return {
      ...IDLE,
      preset: PresetAuto,
      hostId: null,

      async pick(picking, opts) {
        const { phase } = get();
        if (phase !== 'idle' && phase !== 'failed' && phase !== 'picking' && phase !== 'confirming') {
          void picking.then(
            (src) => src?.release(),
            () => undefined,
          );
          return { step: 'cancelled' };
        }
        const mine = ++flow;
        takeWaiting()?.src.release();
        set({ ...IDLE, phase: 'picking', preset: opts.preset });

        let src: PickedSource | null;
        try {
          src = await picking;
        } catch (error) {
          if (flow !== mine) return { step: 'cancelled' };
          set({ phase: 'idle' });
          return { step: 'failed', error };
        }
        if (flow !== mine) {
          src?.release();
          return { step: 'cancelled' };
        }
        if (!src) {
          set({ phase: 'idle' });
          return { step: 'cancelled' };
        }
        if (src.warning === 'screen-with-system-audio') {
          waiting = { src, start: opts.start, unwatch: watchEnded(src) };
          set({ phase: 'confirming', picked: infoOf(src), withAudio: true });
          return { step: 'confirming' };
        }
        return begin(src, opts.start, src.audioScope !== 'none');
      },

      async confirm(withAudio) {
        const taken = get().phase === 'confirming' ? takeWaiting() : null;
        if (!taken) return { step: 'cancelled' };
        const { src, start } = taken;
        if (!withAudio) {
          for (const track of src.preview.getAudioTracks()) {
            track.stop();
            src.preview.removeTrack(track);
          }
        }
        return begin(src, start, withAudio);
      },

      cancel() {
        const { phase } = get();
        if (phase !== 'picking' && phase !== 'confirming') return;
        flow++;
        takeWaiting()?.src.release();
        set(IDLE);
      },

      dismiss() {
        if (get().phase === 'failed') set(IDLE);
      },

      attach(hostId) {
        const host = { id: hostId };
        hosts.push(host);
        if (get().hostId === null) set({ hostId });
        return () => {
          const at = hosts.indexOf(host);
          if (at < 0) return;
          hosts.splice(at, 1);
          if (hosts.length === 0) get().cancel();
          const next = hosts[0]?.id ?? null;
          if (get().hostId !== next) set({ hostId: next });
        };
      },
    };
  });
}

/** The page's share store (one local share per page in M1). Tests make their own with createShareStore(). */
export const shareStore: ShareStore = createShareStore();
