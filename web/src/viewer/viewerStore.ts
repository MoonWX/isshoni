// viewerStore (05 §6.1): what the viewer shows and plays. A vanilla Zustand store, so the controllers that don't
// import React write it (RoomSession feeds it room.state through syncRoom, SubscriberPC its connection state); the
// components read it with useViewer() (viewer/context.ts).
//
// It holds the shares that have a tile, in tile order, with the focus state of autoFocus.ts (which also remembers
// the shares that ended, so that a re-published share gets back the stage and the sound); the audio unlock state
// and volume (05 §10.3, written by audioOut.ts) and whether a tile's video was refused (videoPlayback.ts);
// fullscreen, PiP and visibility (05 §12.4–§12.5, S56); what the server forwards per share (subscribe.status,
// 05 §10.4); and the state of the sub PC (05 §9).
import { createStore, type StoreApi } from 'zustand/vanilla';

import {
  ShareStatusStarting,
  type ParticipantInfo,
  type ShareInfo,
  type SubscriptionStatus,
} from '../protocol/types.gen';
import {
  focusReducer,
  initialFocusState,
  withoutRememberedSound,
  type FocusEvent,
  type FocusShare,
  type FocusState,
} from './autoFocus';

/** Someone who watches a share (ShareInfo.watchers, 01 §8.5), with the name the watchers popover shows. */
export interface ViewerWatcher {
  readonly userId: string;
  /** From room.state's participants; '' when the snapshot has none. */
  readonly name: string;
  /** This user: listed as "you". */
  readonly self: boolean;
}

/** A share that has a tile: live or stalled (shares in `starting` get none, 01 §4.4). */
export interface ViewerShare extends FocusShare {
  /** The share as the last room.state had it. */
  readonly info: ShareInfo;
  /** The sharer's name from room.state's participants; '' when the snapshot has none. */
  readonly ownerName: string;
  /** info.watchers with their names, in the server's order. Nobody watches unseen (05 §12.6). */
  readonly watchers: readonly ViewerWatcher[];
  /**
   * Published by this page: its tile shows the local preview and it is never subscribed (05 §12.2). A share of
   * the same user from another device is `own` but not `local`: it is watched like anyone's.
   */
  readonly local: boolean;
}

/** Who this page is in the room: welcome.user.id and welcome.connectionId. */
export interface ViewerSelf {
  readonly userId: string;
  readonly connectionId: string;
}

/** The parts of room.state the viewer reads. */
export interface RoomSnapshot {
  readonly shares: readonly ShareInfo[];
  readonly participants: readonly Pick<ParticipantInfo, 'userId' | 'name'>[];
}

/**
 * The audio unlock state of 05 §10.3: locked (nothing played yet), playing, blocked (play() was rejected: the
 * browser wants a tap), muted (by the user).
 */
export type AudioUnlockState = 'locked' | 'playing' | 'blocked' | 'muted';

/**
 * The state of the subscribe PeerConnection (05 §9), for the room's banners:
 * - idle: no PC yet (the server offers one with the first subscription);
 * - connecting: the first PC is not connected yet;
 * - connected;
 * - reconnecting: it was connected and is being restarted or rebuilt;
 * - unreachable: 5 rebuilds without connecting: "Can't reach the server's media port. [Test my connection]"; the
 *   rebuilds go on every 30 s;
 * - failed: negotiation failed twice within 60 s: the Fatal screen "Can't connect media" with Reload.
 */
export type SubMediaState = 'idle' | 'connecting' | 'connected' | 'reconnecting' | 'unreachable' | 'failed';

export interface ViewerData extends FocusState<ViewerShare> {
  readonly audio: AudioUnlockState;
  /**
   * The browser refused to play a tile's (muted) video: iOS Low Power Mode. TapToStart then says "Tap to start
   * video" (05 §10.3).
   */
  readonly videoBlocked: boolean;
  /** 0–1; persisted in prefsStore.volume (05 §10.3). */
  readonly volume: number;
  readonly fullscreen: boolean;
  readonly pipShareId: string | null;
  /** Per share: whether its tile is at least 10% visible (IntersectionObserver, 05 §12.4). Absent = not visible. */
  readonly visible: Readonly<Record<string, boolean>>;
  /** Date.now() when the page became hidden; null while it is visible. */
  readonly pageHiddenSince: number | null;
  /** Per share: the last subscribe.status entry (05 §10.4). Absent = nothing reported: normal. */
  readonly status: Readonly<Record<string, SubscriptionStatus>>;
  readonly media: SubMediaState;
}

export interface ViewerActions {
  /**
   * Takes a room.state snapshot (null: not in a room): shares that went live get a tile, ended ones lose theirs, and
   * the focus follows 05 §12.2. A re-published share gets what the share it replaces had (01 §10.6), also when that
   * one ended in an earlier snapshot, as it does after a server restart: the ended shares are remembered until
   * reset(). Per-share state of shares that are gone is dropped.
   */
  syncRoom(room: RoomSnapshot | null, self: ViewerSelf): void;
  /** Runs one focus event through the reducer (autoFocus.ts). */
  dispatch(event: FocusEvent<ViewerShare>): void;
  /** The user picked a tile: focus it and hold it (focusMode 'manual') until that share ends. */
  focusShare(shareId: string): void;
  /** The speaker button (05 §12.3): hear this share without moving the focus; null for silence. */
  setAudible(shareId: string | null): void;
  /**
   * A press on a tile's speaker button: hear that share; pressed while it is the one heard, the sound goes back to
   * the share on the stage (silence when that is a share of this user, which focus never makes audible).
   */
  toggleAudible(shareId: string): void;
  setAudio(audio: AudioUnlockState): void;
  setVideoBlocked(blocked: boolean): void;
  setVolume(volume: number): void;
  setFullscreen(fullscreen: boolean): void;
  setPip(shareId: string | null): void;
  setVisible(shareId: string, visible: boolean): void;
  setPageHidden(since: number | null): void;
  /** Merges subscribe.status entries: only the subscriptions whose status changed are sent (01 §8.9). */
  applyStatus(subs: readonly SubscriptionStatus[]): void;
  setMedia(media: SubMediaState): void;
  /**
   * Back to the state of a page that is in no room: no shares, auto-focus, nothing per share, no fullscreen or PiP,
   * no memory of ended shares. Call it when the user leaves the room, not for a reconnect: after a welcome that was
   * not resumed the room's snapshots keep going to syncRoom, so that re-published shares take over (01 §10.6).
   * What belongs to the page stays: the volume, the audio unlock and the refused videos, the sub PC's state
   * (SubscriberPC.close() resets that one) and the page's visibility.
   */
  reset(): void;
}

export type ViewerState = ViewerData & ViewerActions;
export type ViewerStore = StoreApi<ViewerState>;

export interface ViewerStoreOptions {
  /** prefsStore.volume; default 1. */
  volume?: number;
}

function clamp01(v: number): number {
  return Number.isFinite(v) ? Math.min(1, Math.max(0, v)) : 1;
}

function initialData(volume: number): ViewerData {
  return {
    ...initialFocusState<ViewerShare>(),
    audio: 'locked',
    videoBlocked: false,
    volume,
    fullscreen: false,
    pipShareId: null,
    visible: {},
    pageHiddenSince: null,
    status: {},
    media: 'idle',
  };
}

function focusOf(s: ViewerData): FocusState<ViewerShare> {
  return {
    shares: s.shares,
    focusedShareId: s.focusedShareId,
    focusMode: s.focusMode,
    audibleShareId: s.audibleShareId,
    pendingFocusParam: s.pendingFocusParam,
    ended: s.ended,
  };
}

function sameWatchers(a: ShareInfo, b: ShareInfo): boolean {
  return (
    a.watchers.length === b.watchers.length &&
    a.watchers.every((w, i) => {
      const o = b.watchers[i];
      return o !== undefined && w.userId === o.userId && w.video === o.video && w.audio === o.audio;
    })
  );
}

function watchersOf(info: ShareInfo, names: ReadonlyMap<string, string>, self: ViewerSelf): ViewerWatcher[] {
  return info.watchers.map((w) => ({
    userId: w.userId,
    name: names.get(w.userId) ?? '',
    self: w.userId === self.userId,
  }));
}

/** Whether a tile would render the same from both: the entry of an unchanged share is kept, so tiles don't re-render. */
function sameShare(
  a: ViewerShare,
  info: ShareInfo,
  ownerName: string,
  watchers: readonly ViewerWatcher[],
  self: ViewerSelf,
): boolean {
  const b = a.info;
  return (
    a.ownerName === ownerName &&
    a.watchers.length === watchers.length &&
    a.watchers.every((w, i) => w.name === watchers[i]?.name && w.self === watchers[i].self) &&
    a.own === (info.userId === self.userId) &&
    a.local === (info.connectionId === self.connectionId) &&
    b.userId === info.userId &&
    b.connectionId === info.connectionId &&
    b.status === info.status &&
    b.kind === info.kind &&
    b.label === info.label &&
    b.audio === info.audio &&
    b.preset === info.preset &&
    b.codec === info.codec &&
    b.startedAt === info.startedAt &&
    b.replaces === info.replaces &&
    b.layers.length === info.layers.length &&
    b.layers.every((l, i) => l === info.layers[i]) &&
    sameWatchers(b, info)
  );
}

/**
 * The focus events that turn the known shares into the snapshot's: re-published shares first (one whose replaced
 * share goes in this snapshot takes its place before it goes), then the shares that went, then the new ones.
 */
function roomEvents(
  known: readonly ViewerShare[],
  room: RoomSnapshot | null,
  self: ViewerSelf,
): FocusEvent<ViewerShare>[] {
  const names = new Map(room?.participants.map((p): [string, string] => [p.userId, p.name]));
  const knownById = new Map(known.map((s) => [s.id, s]));
  // Unknown statuses read as live (01 §8.13), so only `starting` is left out.
  const tiled = (room?.shares ?? []).filter((info) => info.status !== ShareStatusStarting);
  const tiledIds = new Set(tiled.map((info) => info.id));

  const replaced: FocusEvent<ViewerShare>[] = [];
  const live: FocusEvent<ViewerShare>[] = [];
  for (const info of tiled) {
    const ownerName = names.get(info.userId) ?? '';
    const watchers = watchersOf(info, names, self);
    const before = knownById.get(info.id);
    if (before && sameShare(before, info, ownerName, watchers, self)) continue;
    const share: ViewerShare = {
      id: info.id,
      userId: info.userId,
      startedAt: info.startedAt,
      own: info.userId === self.userId,
      local: info.connectionId === self.connectionId,
      info,
      ownerName,
      watchers,
    };
    // A re-publish (01 §10.6) that gets its tile now. The reducer knows what the replaced share had, whether it
    // still has its tile or ended in an earlier snapshot (after a server restart the first room.state has no
    // shares, and the new share is `starting` before it is live), and checks that both are of the same user.
    if (!before && info.replaces !== undefined && info.replaces !== '') {
      replaced.push({ type: 'shareReplaced', share, replaces: info.replaces });
    } else {
      live.push({ type: 'shareLive', share });
    }
  }
  // One event for all that went, so each is remembered as it was before this snapshot.
  const gone = known.filter((s) => !tiledIds.has(s.id)).map((s) => s.id);
  const ended: FocusEvent<ViewerShare>[] = gone.length > 0 ? [{ type: 'shareEnded', shareIds: gone }] : [];
  // New shares oldest first: room.state lists them by startedAt, and each one that is newer takes the focus in turn.
  return [...replaced, ...ended, ...live];
}

function pruned<T>(map: Readonly<Record<string, T>>, keep: ReadonlySet<string>): Readonly<Record<string, T>> {
  const keys = Object.keys(map);
  if (keys.every((k) => keep.has(k))) return map;
  return Object.fromEntries(Object.entries(map).filter(([k]) => keep.has(k)));
}

export function createViewerStore(opts: ViewerStoreOptions = {}): ViewerStore {
  return createStore<ViewerState>()((set, get) => {
    const dispatchAll = (events: readonly FocusEvent<ViewerShare>[]): void => {
      if (events.length === 0) return;
      set((s) => {
        const before = focusOf(s);
        const after = events.reduce<FocusState<ViewerShare>>((state, event) => focusReducer(state, event), before);
        if (after === before) return s;
        const ids = new Set(after.shares.map((share) => share.id));
        return {
          ...after,
          visible: pruned(s.visible, ids),
          status: pruned(s.status, ids),
          pipShareId: s.pipShareId !== null && !ids.has(s.pipShareId) ? null : s.pipShareId,
        };
      });
    };

    return {
      ...initialData(clamp01(opts.volume ?? 1)),

      syncRoom(room, self) {
        dispatchAll(roomEvents(get().shares, room, self));
      },
      dispatch(event) {
        dispatchAll([event]);
      },
      focusShare(shareId) {
        dispatchAll([{ type: 'userFocus', shareId }]);
      },
      setAudible(shareId) {
        const s = get();
        // Only a share with a tile that this page receives can be heard: never the own preview.
        if (shareId !== null && !s.shares.some((share) => share.id === shareId && !share.local)) return;
        // The user chose what to hear: a share that comes back later doesn't take the sound from that choice.
        const ended = withoutRememberedSound(s.ended);
        if (s.audibleShareId === shareId && ended === s.ended) return;
        set({ audibleShareId: shareId, ended });
      },
      toggleAudible(shareId) {
        const s = get();
        if (s.audibleShareId !== shareId) {
          s.setAudible(shareId);
          return;
        }
        const focused = s.shares.find((share) => share.id === s.focusedShareId);
        s.setAudible(focused && !focused.own && focused.id !== shareId ? focused.id : null);
      },
      setAudio(audio) {
        if (get().audio !== audio) set({ audio });
      },
      setVideoBlocked(videoBlocked) {
        if (get().videoBlocked !== videoBlocked) set({ videoBlocked });
      },
      setVolume(volume) {
        set({ volume: clamp01(volume) });
      },
      setFullscreen(fullscreen) {
        set({ fullscreen });
      },
      setPip(pipShareId) {
        set({ pipShareId });
      },
      setVisible(shareId, visible) {
        if ((get().visible[shareId] ?? false) === visible) return;
        set((s) => ({ visible: { ...s.visible, [shareId]: visible } }));
      },
      setPageHidden(pageHiddenSince) {
        set({ pageHiddenSince });
      },
      applyStatus(subs) {
        if (subs.length === 0) return;
        set((s) => {
          const status = { ...s.status };
          for (const sub of subs) status[sub.shareId] = sub;
          return { status };
        });
      },
      setMedia(media) {
        if (get().media !== media) set({ media });
      },
      reset() {
        set({ ...initialFocusState<ViewerShare>(), fullscreen: false, pipShareId: null, visible: {}, status: {} });
      },
    };
  });
}
