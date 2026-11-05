// The web → desktop handoff (docs/m2/03 §12.2, §12.3): what the same-user relay (01 §8.14, the kinds of
// internal/protocol/relay, typed by relay.gen.ts) carries between the user's browser tabs and their desktop app, and
// the user's own connections (user.connections, M2 01 §10.4) that both sides need. The runtime attaches it once per
// page (runtime.ts), for the life of the runtime, so it works while the room page isn't mounted (05 §11.1). No React
// here (05 §3).
//
// - user.connections: the list is kept in the handoff store (who is online, which of them is a desktop app). In the
//   desktop app the online ids go to Go on every change (App.OwnConnections), which expires the playback.state of a
//   tab that left (03 §12.2 "Expiry").
// - playback.stop, sender (the desktop app's page, C1): once each time the user brings the app into a room, i.e. the
//   first room.state of the room after a join() that asked for a room the session wasn't in (RoomSession's 'join':
//   the user, a room page, a deep link), never after a resync's re-join, so a tab whose user chose "Watch here
//   instead" isn't stopped again by a network blip. A deep link to the room the app is already in asks for it again
//   (requestStop). Sent with toRole full: the user's browser tabs that can share; phones are viewers and keep playing.
//   agent_target_not_found (no such tab) is normal; any other error is logged at debug.
// - playback.stop, receiver (every browser tab): a payload with v 1 and the session's room, from a connection that
//   the user's own participant entry of room.state lists with kind desktop, hands the tab off: viewerStore.handedOff,
//   so nothing is subscribed (layerPolicy rule 9) and the stage says "Now watching in the isshoni app" with Watch
//   here instead. A sender that the snapshot doesn't list yet (its room.state may come after the message) is checked
//   again with the snapshots of the next HANDOFF_VERIFY_MS. Anything else is ignored, like an unknown kind. The state
//   ends with a room switch (viewerStore.reset) or a reload.
// - playback.state, sender (every browser tab): {playing, browser, os} to each online desktop connection in
//   user.connections, addressed with `to` (never toRole, 01 §11.11), whenever the tab's audible playback starts or
//   stops (a subscription with audio on, and the element unmuted and playing), and again to a desktop connection
//   that newly appears in the list (and once more PLAYBACK_STATE_REPEAT_MS later), and to all of them after a
//   welcome (the tab's connection id may be new). Errors are ignored like playback.stop's. Go's App gets it through
//   C2 (01's EventPlaybackState) and keeps the browser out of the share (05 §5.9); the desktop's C1 ignores the kind.
//
// Relay payloads are never logged (01 §11.12): debug lines name the kind only.
import { createStore, type StoreApi } from 'zustand/vanilla';

import type { Logger } from '../lib/log';
import type { Platform } from '../platform/types';
import { isProtocolError } from '../protocol/errors';
import {
  BrowserChrome,
  BrowserEdge,
  BrowserFirefox,
  BrowserOther,
  KindPlaybackState,
  KindPlaybackStop,
  PlaybackStopVersion,
  type Browser,
  type PlaybackState,
  type PlaybackStop,
} from '../protocol/relay.gen';
import type { SignalClient } from '../protocol/signal-client';
import {
  ClientKindDesktop,
  ConnectionStatusReconnecting,
  ErrorCodeAgentTargetNotFound,
  FeatureAgentRelay,
  MessageTypeAgentRecv,
  MessageTypeAgentSend,
  MessageTypeUserConnections,
  RoleFull,
  type AgentRecv,
  type ConnectionInfo,
  type OwnConnection,
  type RoomState,
} from '../protocol/types.gen';
import { viewerWants } from '../viewer/subscriptions';
import type { ViewerData, ViewerStore } from '../viewer/viewerStore';
import type { RoomSession } from './RoomSession';
import type { RoomStore } from './roomStore';

/**
 * A desktop connection that appears in user.connections hears the tab's playback.state again after this long. The
 * app's Go side follows C2's events only once C2 is online, so the report that the tab sends as soon as C2 appears
 * can reach C2 before anyone listens (a race in App.followPlayback, seen in desktop-handoff.spec); the repeat makes
 * sure it lands. Repeating the same value changes nothing in Go.
 */
export const PLAYBACK_STATE_REPEAT_MS = 3_000;

/** How long a playback.stop whose sender room.state doesn't list yet waits for a snapshot that does. */
export const HANDOFF_VERIFY_MS = 3_000;

// ---- The handoff store ----

export interface HandoffState {
  /**
   * The last user.connections: every connection of the user, this one included, sorted by id. null before the first
   * one, when the server doesn't serve the feature, and after the connection stopped.
   */
  readonly connections: readonly OwnConnection[] | null;
}

export type HandoffStore = StoreApi<HandoffState>;

/** A connection counts as online unless it is reconnecting (01: clients treat unknown statuses as online). */
export function isOnline(c: Pick<ConnectionInfo, 'status'>): boolean {
  return c.status !== ConnectionStatusReconnecting;
}

/** The user's desktop app connections that are online: C1 (the app's page) and C2 (its publisher). */
export function onlineDesktops(connections: readonly OwnConnection[] | null): OwnConnection[] {
  return (connections ?? []).filter((c) => c.kind === ClientKindDesktop && isOnline(c));
}

/** M1's client.browser as one of playback.state's four families (03 §12.2): safari and anything else are other. */
export function browserFamily(browser: string | undefined): Browser {
  switch (browser) {
    case BrowserChrome:
    case BrowserEdge:
    case BrowserFirefox:
      return browser;
    default:
      return BrowserOther;
  }
}

/**
 * Whether the tab plays sound now (03 §12.2): a subscription with audio on (the layer policy's set, so neither a
 * handed-off tab nor the endpoint mute counts) and an element that plays unmuted.
 */
export function playsAudibly(s: ViewerData): boolean {
  return s.audio === 'playing' && viewerWants(s).some((w) => w.audio === 'on');
}

/**
 * Whether a playback.stop payload is one this tab follows, apart from its sender (03 §12.2): v 1 and the session's
 * room. Unknown fields are ignored.
 */
export function stopForRoom(payload: unknown, roomId: string | null): boolean {
  if (roomId === null || typeof payload !== 'object' || payload === null || Array.isArray(payload)) return false;
  const p = payload as Partial<Record<keyof PlaybackStop, unknown>>;
  return p.v === PlaybackStopVersion && typeof p.roomId === 'string' && p.roomId === roomId;
}

/** Whether `from` is a desktop connection of the user's own participant entry in the snapshot. */
export function fromOwnDesktop(state: RoomState | null, userId: string | null, from: string): boolean {
  if (state === null || userId === null) return false;
  const self = state.participants.find((p) => p.userId === userId);
  return self?.connections.some((c) => c.id === from && c.kind === ClientKindDesktop) === true;
}

// ---- attachHandoff ----

export interface HandoffDeps {
  platform: Pick<Platform, 'client' | 'desktop'>;
  signal: Pick<SignalClient, 'on' | 'onState' | 'request' | 'featureActive' | 'state'>;
  session: Pick<RoomSession, 'on' | 'roomId'>;
  room: RoomStore;
  viewer: ViewerStore;
  log: Logger;
}

export interface Handoff {
  readonly store: HandoffStore;
  /**
   * The desktop app's page (C1) sends playback.stop for its room again once that room's snapshot is there (now when
   * it is): a deep link to the room the app is already in (03 §13.2). Nothing in the browser.
   */
  requestStop(): void;
  /** Stops listening. */
  dispose(): void;
}

/** Wires the handoff for one page (see the header). */
export function attachHandoff(deps: HandoffDeps): Handoff {
  const { platform, signal, session, room, viewer, log } = deps;
  const bridge = platform.desktop?.bridge ?? null;
  const desktop = bridge !== null;
  const store = createStore<HandoffState>()(() => ({ connections: null }));
  const offs: (() => void)[] = [];
  let disposed = false;

  /** A relay message failed: no tab is normal, anything else is worth a debug line (with the kind only). */
  const relayFailed = (kind: string) => (err: unknown) => {
    if (isProtocolError(err) && err.code === ErrorCodeAgentTargetNotFound) return;
    log.debug('relay message not delivered', { kind, code: isProtocolError(err) ? err.code : 'local' });
  };

  // ---- user.connections ----

  /** The online ids last given to App.OwnConnections, joined: Go hears only a change. */
  let toldOwn: string | null = null;
  offs.push(
    signal.on(MessageTypeUserConnections, (msg) => {
      const connections = Array.isArray(msg.connections) ? msg.connections : [];
      store.setState({ connections });
      if (bridge === null) return;
      const ids = connections.filter(isOnline).map((c) => c.id);
      const key = ids.join(' ');
      if (key === toldOwn) return;
      toldOwn = key;
      bridge.app.ownConnections(ids);
    }),
    signal.onState((state) => {
      // A new connection gets a new list after its welcome; until then nobody is known.
      if (state === 'stopped') store.setState({ connections: null });
    }),
  );

  // ---- playback.stop: the desktop app's sender ----

  /** The room whose first snapshot sends playback.stop; null when none is due. */
  let stopDue: string | null = null;
  const sendStop = (roomId: string): void => {
    stopDue = null;
    if (!signal.featureActive(FeatureAgentRelay)) {
      log.debug('relay not active: no playback.stop');
      return;
    }
    const payload: PlaybackStop = { v: PlaybackStopVersion, roomId };
    signal
      .request(MessageTypeAgentSend, { toRole: RoleFull, kind: KindPlaybackStop, payload })
      .catch(relayFailed(KindPlaybackStop));
  };
  /** Sends the due stop once the snapshot is of its room. */
  const maybeStop = (state: RoomState | null): void => {
    if (stopDue !== null && state !== null && state.roomId === stopDue && signal.state === 'ready') sendStop(stopDue);
  };
  if (desktop) {
    offs.push(
      session.on('join', (roomId) => {
        stopDue = roomId;
        // join() clears the snapshot of another room; for the same room the next snapshot sends it.
      }),
    );
  }

  // ---- playback.stop: the tabs' receiver ----

  /** A stop whose sender the snapshot didn't list yet: checked again with the snapshots until `until`. */
  let unverified: { from: string; roomId: string; until: number } | null = null;
  const handOff = (): void => {
    unverified = null;
    if (viewer.getState().handedOff) return;
    log.info('handed off to the desktop app');
    viewer.getState().setHandedOff(true);
  };
  const verify = (): void => {
    if (unverified === null) return;
    if (Date.now() > unverified.until || unverified.roomId !== session.roomId) {
      unverified = null;
      return;
    }
    const s = room.getState();
    if (s.state?.roomId === unverified.roomId && fromOwnDesktop(s.state, s.userId, unverified.from)) handOff();
  };
  if (!desktop) {
    offs.push(
      signal.on(MessageTypeAgentRecv, (msg: AgentRecv) => {
        if (msg.kind !== KindPlaybackStop) return;
        const roomId = session.roomId;
        if (!stopForRoom(msg.payload, roomId) || roomId === null || typeof msg.from !== 'string') {
          log.debug('relay message ignored', { kind: msg.kind });
          return;
        }
        unverified = { from: msg.from, roomId, until: Date.now() + HANDOFF_VERIFY_MS };
        verify();
      }),
    );
  }

  // ---- playback.state: the tabs' sender ----

  /** The value the desktops in `told` have; null before the first. */
  let playing: boolean | null = null;
  /** The desktop connections that were sent `playing`. */
  const told = new Set<string>();
  /** The repeats due for desktop connections that appeared (PLAYBACK_STATE_REPEAT_MS). */
  const repeats = new Set<ReturnType<typeof setTimeout>>();
  const sendState = (): void => {
    if (desktop || playing === null || signal.state !== 'ready' || !signal.featureActive(FeatureAgentRelay)) return;
    const payload: PlaybackState = {
      playing,
      browser: browserFamily(platform.client.browser),
      os: platform.client.os,
    };
    const online = onlineDesktops(store.getState().connections);
    for (const c of online) {
      if (told.has(c.id)) continue;
      told.add(c.id);
      signal
        .request(MessageTypeAgentSend, { to: c.id, kind: KindPlaybackState, payload })
        .catch(relayFailed(KindPlaybackState));
    }
    // A desktop that went away gets the value again when it comes back.
    for (const id of told) if (!online.some((c) => c.id === id)) told.delete(id);
  };
  const followPlayback = (s: ViewerData): void => {
    const now = playsAudibly(s);
    if (now === playing) return;
    playing = now;
    told.clear();
    sendState();
  };
  if (!desktop) {
    followPlayback(viewer.getState());
    offs.push(
      viewer.subscribe((s) => {
        followPlayback(s);
      }),
      store.subscribe((s, prev) => {
        if (s.connections === prev.connections) return;
        sendState();
        // A desktop connection that just appeared hears the value once more a little later: the app subscribes to
        // its C2 only after C2's welcome, so a report that comes right with it can be missed (see the header).
        const before = new Set(onlineDesktops(prev.connections).map((c) => c.id));
        for (const c of onlineDesktops(s.connections)) {
          if (before.has(c.id)) continue;
          const timer = setTimeout(() => {
            repeats.delete(timer);
            told.delete(c.id);
            sendState();
          }, PLAYBACK_STATE_REPEAT_MS);
          repeats.add(timer);
        }
      }),
      signal.onState((state) => {
        // After a welcome the app may know this tab by a new connection id: everyone hears the value again.
        if (state !== 'ready') return;
        told.clear();
        sendState();
      }),
    );
  }

  // ---- Snapshots ----

  offs.push(
    room.subscribe((s, prev) => {
      if (s.state === prev.state) return;
      maybeStop(s.state);
      verify();
    }),
  );

  return {
    store,
    requestStop() {
      if (!desktop || disposed) return;
      const roomId = session.roomId;
      if (roomId === null) return;
      stopDue = roomId;
      maybeStop(room.getState().state);
    },
    dispose() {
      disposed = true;
      for (const off of offs.splice(0)) off();
      for (const timer of repeats) clearTimeout(timer);
      repeats.clear();
    },
  };
}
