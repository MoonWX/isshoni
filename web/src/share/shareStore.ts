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
// The store drives the first half itself: idle, picking, confirming, and the hand-over to `start` (the room
// session's startShare, which publishes through platform.sharing.start). The second half is the publisher's
// (BrowserSharing and its BrowserShare): through `publishing`, `advance`, `report` and `finish` it moves `starting`
// to `live`, `reconnecting`, `stopping`, `idle` or `failed`, and fills `params` and `hint`. M1 has one local share
// per page (05 §13.1), so one store per page: `shareStore`.
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

/** Whether a share is under way in this phase: published, being published, or being stopped. */
export function isSharingPhase(phase: SharePhase): boolean {
  return phase === 'starting' || phase === 'live' || phase === 'reconnecting' || phase === 'stopping';
}

/**
 * Something to tell the user about a share that ended without being a failure (05 §13.1):
 * `elsewhere`: "Sharing was stopped from another tab or device". Each notice is a new object, so a listener can
 * tell a second one from the first.
 */
export interface ShareNotice {
  readonly kind: 'elsewhere';
}

/** How a share ended, for finish(): nothing (→ idle), an error (→ failed), or a notice (→ idle, and a toast). */
export interface ShareOutcome {
  readonly error?: unknown;
  readonly notice?: ShareNotice['kind'];
}

/** What was picked (05 §13.3), without the stream. */
export type PickedInfo = Pick<PickedSource, 'kind' | 'audioScope' | 'warning'>;

/**
 * Publishes a picked source: the room session's startShare (05 §11.1). From the call on, the source is the
 * callee's; when it rejects, the store releases the source and goes to `failed`, or, for a ShareCancelledError,
 * back to `idle` without a word.
 */
export type StartShare = (src: PickedSource, opts: { preset: Preset; withAudio: boolean }) => Promise<unknown>;

/**
 * A start that was given up, not one that failed: the capture ended while the share was being started (the
 * browser's own "Stop sharing" bar, or the shared window closing). The user did that, so there is nothing to report.
 */
export class ShareCancelledError extends Error {
  override readonly name = 'ShareCancelledError';

  constructor() {
    super('the capture ended while the share was starting');
  }
}

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
  /** The sound switch of the share panel (05 §13.7): false while a share with sound sends silence. */
  readonly soundOn: boolean;
  /** The latest ShareParams (share.start, share.update, quality.hint). Filled by the publisher. */
  readonly params: ShareParams | null;
  /** The upload or CPU hint of 05 §13.7. Filled by the publisher. */
  readonly hint: ShareHint | null;
  /** The pub PC can't reach the server's media port (01 §10.4: 5 rebuilds without connecting). */
  readonly unreachable: boolean;
  /** Why the phase is `failed`; null otherwise. */
  readonly error: unknown;
  /** The last notice of a share that ended (see ShareNotice); null until there is one, and from the next pick on. */
  readonly notice: ShareNotice | null;
  /**
   * The attached host that shows the ScreenAudioWarning: the one attached longest; null while none is attached.
   * Whichever button's click started the pick, exactly one mounted button shows its warning.
   */
  readonly hostId: string | null;
  /**
   * How many SharePanels are mounted. A panel shows a failed share and the "no sound is shared" note itself, so
   * while one is there the Share buttons leave those to it (and say them as toasts otherwise).
   */
  readonly panels: number;
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
  /** A SharePanel mounts (see `panels`); returns its detach, for the unmount. */
  attachPanel(): () => void;

  // ---- The publisher's half ----

  /**
   * A share is being published (share.start was answered): → starting, with what is shared and how. After a pick
   * through this store the phase is `starting` already and only the fields are filled in.
   *
   * Returns false, and changes nothing, while another share is already published or being stopped: the machine
   * follows one share (05 §13.1), and that other share's advance, report and finish are the ones that count.
   */
  publishing(share: { picked: PickedInfo; preset: Preset; withAudio: boolean; params: ShareParams }): boolean;
  /**
   * starting → live (the first keyframe: the share is live in room.state); live ⇄ reconnecting (the pub PC);
   * any of them → stopping. Ignored while no share is under way, and nothing leaves stopping but finish().
   */
  advance(phase: 'live' | 'reconnecting' | 'stopping'): void;
  /** What changes while a share is under way. Ignored otherwise. */
  report(
    change: Partial<Pick<ShareFields, 'preset' | 'withAudio' | 'soundOn' | 'params' | 'hint' | 'unreachable'>>,
  ): void;
  /**
   * The share is over: → idle, or → failed with outcome.error; outcome.notice is kept for whoever tells the user.
   * Ignored while no share is under way (a start that failed before publishing is the first half's to report).
   */
  finish(outcome?: ShareOutcome): void;
}

export type ShareState = ShareFields & ShareActions;
export type ShareStore = StoreApi<ShareState>;

/**
 * Everything but the preset, which outlives a share; the host and the panels, which are about the page and not the
 * share; and the notice, which stays until the next pick.
 */
const IDLE: Omit<ShareFields, 'preset' | 'hostId' | 'panels' | 'notice'> = Object.freeze({
  phase: 'idle',
  withAudio: false,
  picked: null,
  soundOn: true,
  params: null,
  hint: null,
  unreachable: false,
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
  /** A share reported itself with publishing() and hasn't finished: the machine is following that one. */
  let published = false;
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
      published = false;
      const picked = infoOf(src);
      set({ phase: 'starting', picked, withAudio, error: null });
      try {
        await start(src, { preset: get().preset, withAudio });
      } catch (error) {
        src.release();
        // Unless something else (the publisher, a newer pick) has moved the machine on meanwhile.
        const current = flow === mine && get().phase === 'starting';
        if (error instanceof ShareCancelledError) {
          if (current) set(IDLE);
          return { step: 'cancelled' };
        }
        if (current) set({ phase: 'failed', error });
        return { step: 'failed', error };
      }
      return { step: 'started', picked, withAudio };
    };

    return {
      ...IDLE,
      preset: PresetAuto,
      hostId: null,
      panels: 0,
      notice: null,

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
        set({ ...IDLE, phase: 'picking', preset: opts.preset, notice: null });

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

      attachPanel() {
        let attached = true;
        set({ panels: get().panels + 1 });
        return () => {
          if (!attached) return;
          attached = false;
          set({ panels: get().panels - 1 });
        };
      },

      publishing(share) {
        const { phase } = get();
        // Not over a share the machine already follows (the M1 UI starts one, 05 §13.1).
        if (published && isSharingPhase(phase)) return false;
        published = true;
        if (phase !== 'starting') {
          // Started without a pick through this store: whatever was being picked here is given up.
          flow++;
          takeWaiting()?.src.release();
        }
        set({ ...IDLE, ...share, phase: 'starting', soundOn: share.withAudio });
        return true;
      },

      advance(phase) {
        const now = get().phase;
        if (!isSharingPhase(now) || now === 'stopping' || now === phase) return;
        // Only a share that was live can be reconnecting; one that is still starting stays starting.
        if (phase === 'reconnecting' && now !== 'live') return;
        set({ phase });
      },

      report(change) {
        if (isSharingPhase(get().phase)) set(change);
      },

      finish(outcome = {}) {
        if (!isSharingPhase(get().phase)) return;
        flow++;
        published = false;
        if (outcome.error !== undefined) set({ ...IDLE, phase: 'failed', error: outcome.error, picked: get().picked });
        else set({ ...IDLE, ...(outcome.notice !== undefined ? { notice: { kind: outcome.notice } } : {}) });
      },
    };
  });
}

/** The page's share store (one local share per page in M1). Tests make their own with createShareStore(). */
export const shareStore: ShareStore = createShareStore();
