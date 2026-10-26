// DesktopSharing (docs/m2/03 §10.2): the desktop app's SharingProvider, mode 'native'. The engine captures and C2,
// the app's own publisher connection (§9), publishes; this page only asks Go through the bound Share service and
// mirrors the share controller's view (the share.state event, pulled once with Share.Current()):
// - pick() → Share.Pick(): the OS picker owned by the app's window. The PickedSource carries no stream (preview null),
//   audioScope from the sheet's Share sound switch, the picker's name (label, shown on this PC only) and the one-time
//   sheets due (confirm). Go holds the pick for 300 s; release() gives it back.
// - start() → Share.Start({pickId, roomId, preset, withAudio}): it resolves at `starting` with the share's
//   DesktopShare; `live` comes as an event. ctx.signal is unused (C2 publishes).
// - current(): the share Go publishes now, also one that outlived this page (a reload, a WebView2 crash): the room
//   runtime adopts it into the room session (RoomSession.adoptShare). null until the first view arrived.
// - localConnectionIds(): C2's connection id (App.Info().publisherConnectionId, and each view's connectionId), so the
//   room page counts the app's own share as local (the viewer, findShareElsewhere).
// - stopOther(roomId, shareId) → Share.StopOther: C2 sends share.stop for another connection's share of this user,
//   which this page's own connection may not (its role is viewer, 01 §6.3).
//
// Views reach it on two paths: events (ordered among themselves) and the answers of Share.Start and Share.Current()
// (which can arrive after newer events). A pulled view is applied only when no event came since its call, so it never
// rolls a share back to an older state; Share.Start's answer makes the share unless one exists already or it ended
// since the call. While a share runs, the controller's Current() is never null nor the picker's view (Pick answers
// already_sharing), so such an event then is an old one that arrived late: it only makes this provider pull again.
//
// It loads on demand, in a chunk of its own (lazySharing.ts keeps platform.sharing in the entry chunk tiny), because it
// writes the page's share store, which belongs to share/ and not to the entry chunk.
import { LocalError } from '../../lib/errors';
import { createLogger, type Logger } from '../../lib/log';
import type { Preset } from '../../protocol/types.gen';
import { shareStore, type ShareStore } from '../../share/shareStore';
import type { ActiveShare, PickedSource, ShareContext, SharingProvider } from '../types';
import type { NativeBridge, ShareView } from './bridge';
import { DesktopShare, isActiveView, isFinalView, viewError } from './DesktopShare';
import { BridgeError } from './errors';

export interface DesktopSharingOptions {
  /** The share state machine the shares report to. Default: the page's shareStore; null: none. */
  store?: ShareStore | null;
  log?: Logger;
}

/** The pick a PickedSource stands for. */
interface Pick {
  readonly pickId: string;
}

export class DesktopSharing implements SharingProvider {
  readonly mode = 'native' as const;
  readonly #bridge: NativeBridge;
  readonly #store: ShareStore | null;
  readonly #log: Logger;
  /** The picks handed out, by their PickedSource. */
  readonly #picks = new WeakMap<PickedSource, Pick>();
  readonly #ready: Promise<void>;
  /** The share Go publishes now; null when there is none (or no view has arrived yet). */
  #share: DesktopShare | null = null;
  /** The last final view (ended, failed): what start() reports when its share ended before it answered. */
  #lastFinal: ShareView | null = null;
  /** C2's connection id: App.Info's, then each view's. */
  #publisherId = '';
  /** share.state events so far: a pulled view is applied only when none came after its call. */
  #events = 0;
  /** Final views so far: Share.Start's answer makes no share when its share ended since the call. */
  #finals = 0;

  constructor(bridge: NativeBridge, opts: DesktopSharingOptions = {}) {
    this.#bridge = bridge;
    this.#store = opts.store === undefined ? shareStore : opts.store;
    this.#log = opts.log ?? createLogger('share');
    // Subscribe first, then pull (docs/m2/03 §6.3): nothing is missed.
    bridge.on('share.state', (view) => {
      this.#events++;
      this.#apply(view, false);
    });
    bridge.on('share.notice', (notice) => {
      this.#share?.notify(notice);
    });
    this.#ready = this.#pull();
    bridge.app
      .info()
      .then((info) => {
        if (info.publisherConnectionId !== '' && this.#publisherId === '')
          this.#publisherId = info.publisherConnectionId;
      })
      .catch((err: unknown) => {
        this.#log.warn('App.Info failed', { error: err });
      });
  }

  /** Resolves once the first pull of Share.Current() was answered (tests, and whoever needs current() settled). */
  ready(): Promise<void> {
    return this.#ready;
  }

  /**
   * Share.Pick(): null when the picker was closed. Rejects with the bridge's error: already_sharing,
   * engine.<code> (engine.invalid with params.reason self_window for an isshoni window, docs/m2/03 §10.5).
   */
  async pick(opts: { preset: Preset; withAudio?: boolean }): Promise<PickedSource | null> {
    const picked = await this.#bridge.share.pick();
    if (picked === null) return null;
    const { pickId } = picked;
    const bridge = this.#bridge;
    let released = false;
    const src: PickedSource = {
      kind: picked.kind,
      audioScope: opts.withAudio === false ? 'none' : 'system',
      warning: null,
      preview: null,
      label: picked.label,
      confirm: [...picked.confirm],
      release() {
        if (released) return;
        released = true;
        bridge.share.release(pickId);
      },
    };
    this.#picks.set(src, { pickId });
    return src;
  }

  /**
   * Share.Start: resolves with the share's DesktopShare at `starting`. Rejects with the bridge's error
   * (pick_expired, already_sharing, not_linked, publisher_offline, engine.<code>, wire codes), or with a LocalError
   * capture_failed for a source this provider didn't pick.
   */
  async start(
    src: PickedSource,
    opts: { preset: Preset; withAudio: boolean },
    ctx: ShareContext,
  ): Promise<ActiveShare> {
    const pick = this.#picks.get(src);
    if (pick === undefined) throw new LocalError('capture_failed');
    const finals = this.#finals;
    const view = await this.#bridge.share.start({
      pickId: pick.pickId,
      roomId: ctx.roomId,
      preset: opts.preset,
      withAudio: opts.withAudio,
    });
    if (view.connectionId !== '') this.#publisherId = view.connectionId;
    // The answer, unless events have told more meanwhile: the share may be live already, or over.
    if (this.#share === null && this.#finals === finals) this.#apply(view, true);
    const share = this.#share;
    if (share === null) {
      // It ended before this answer was read: say why, as a start that failed.
      throw viewError(this.#lastFinal) ?? new BridgeError('not_sharing');
    }
    return share;
  }

  /** The share Go publishes now (not one that ended); null while there is none. */
  current(): ActiveShare | null {
    return this.#share;
  }

  /** C2's connection id, whose shares are this page's own (docs/m2/03 §10.4); [] while it isn't known. */
  localConnectionIds(): string[] {
    const ids = [this.#share?.view.connectionId ?? '', this.#publisherId].filter((id) => id !== '');
    return [...new Set(ids)];
  }

  /** Share.StopOther: C2 sends share.stop for another connection's share of this user. */
  stopOther(roomId: string, shareId: string): Promise<void> {
    return this.#bridge.share.stopOther(roomId, shareId);
  }

  /** Pulls Share.Current() and applies it unless an event came first. */
  #pull(): Promise<void> {
    const seen = this.#events;
    return this.#bridge.share
      .current()
      .then((view) => {
        if (this.#events === seen) this.#apply(view, true);
      })
      .catch((err: unknown) => {
        this.#log.warn('Share.Current failed', { error: err });
      });
  }

  /**
   * One view of the controller (answer: from Share.Start or Share.Current(), else an event): a share starts, changes
   * or ends.
   */
  #apply(view: ShareView | null, answer: boolean): void {
    const share = this.#share;
    if (view !== null && isFinalView(view.state)) {
      this.#finals++;
      this.#lastFinal = view;
      if (share !== null) {
        this.#share = null;
        share.end(view);
      }
      return;
    }
    if (view === null || !isActiveView(view.state)) {
      if (share === null) return;
      if (!answer) {
        // An old event that arrived late (see the file's comment): ask Go what is true now.
        void this.#pull();
        return;
      }
      // Go says there is no share now: this one is over, without a final view.
      this.#share = null;
      share.end(null);
      return;
    }
    if (view.connectionId !== '') this.#publisherId = view.connectionId;
    if (share !== null) {
      share.update(view);
      return;
    }
    this.#share = new DesktopShare({
      bridge: this.#bridge,
      view,
      store: this.#store,
      refresh: () => {
        void this.#pull();
      },
      log: this.#log,
    });
  }
}
