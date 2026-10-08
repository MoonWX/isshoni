// Test support for rooms/ (05 §19.1): a room runtime (the real SignalClient, RoomSession and stores) against
// protocol/testing's fake signaling server, with Vitest's fake timers, plus a fake local share and a fake sub PC.
// Only tests import this folder.
import { vi } from 'vitest';

import type { AppServices } from '../../app/context';
import type {
  ActiveShare,
  LocalEndReason,
  LocalShareState,
  PickedSource,
  Platform,
  ShareContext,
  ShareHint,
  SharingProvider,
} from '../../platform/types';
import { FakeSignalServer, type ClientMessage, type FakeSignalServerOptions } from '../../protocol/testing';
import type {
  EndReason,
  ParticipantInfo,
  PCICE,
  PCOffer,
  Preset,
  RoomState,
  ShareInfo,
  SubscribeUpdate,
  SubscriptionWant,
} from '../../protocol/types.gen';
import { createTestPlatform } from '../../test/platform';
import { createTestServices } from '../../test/render';
import type { ShareRecovery, ShareResyncContext, SubscriberDeps, SubscriberLike } from '../RoomSession';
import { getRoomRuntime, type RoomRuntime, type RoomRuntimeOptions } from '../runtime';

/** Advances the fake clock; the fake server's automatic steps run at +0 ms. */
export async function tick(ms = 0): Promise<void> {
  await vi.advanceTimersByTimeAsync(ms);
}

/** Tracks a promise's outcome synchronously (and marks its rejection handled). */
export function track<T>(p: Promise<T>): { done: boolean; value?: T; error?: unknown } {
  const r: { done: boolean; value?: T; error?: unknown } = { done: false };
  void p.then(
    (value) => {
      r.done = true;
      r.value = value;
    },
    (error: unknown) => {
      r.done = true;
      r.error = error;
    },
  );
  return r;
}

export function participant(userId: string, name: string, overrides: Partial<ParticipantInfo> = {}): ParticipantInfo {
  return {
    userId,
    name,
    status: 'present',
    joinedAt: '2026-10-12T19:00:01.000Z',
    connections: [{ id: `c_${userId}`, kind: 'web', role: 'full', status: 'online' }],
    ...overrides,
  };
}

export function shareInfo(
  id: string,
  userId: string,
  connectionId: string,
  overrides: Partial<ShareInfo> = {},
): ShareInfo {
  return {
    id,
    userId,
    connectionId,
    kind: 'window',
    preset: 'auto',
    audio: true,
    status: 'live',
    layers: ['high', 'low'],
    startedAt: '2026-10-12T19:02:30.000Z',
    watchers: [],
    ...overrides,
  };
}

export function roomState(roomId: string, rev: number, overrides: Partial<RoomState> = {}): RoomState {
  return { roomId, rev, participants: [], shares: [], ...overrides };
}

/**
 * The fake server's room side: answers room.join (ok, then a room.state like the hub's), room.leave, share.stop and
 * subscribe.update, and records them. Tests replace a handler with server.handle() where they need another answer.
 */
export class FakeHub {
  /** Room names by id; a room.join of any other id is still answered ok, with the id as the name. */
  readonly names: Record<string, string> = { lounge: 'Lounge', games: 'Games' };
  /** The next rev (one hub-wide counter, 01 §8.5). */
  rev = 1;
  /** The shares put into the room.state that follows a join. */
  shares: ShareInfo[] = [];
  /** shareIds that subscribe.update reports as ignored. */
  ignored = new Set<string>();

  readonly #server: FakeSignalServer;

  constructor(server: FakeSignalServer) {
    this.#server = server;
    server.handle('room.join', (data) => {
      queueMicrotask(() => {
        this.sendState(data.roomId);
      });
      return { ok: { room: { id: data.roomId, name: this.names[data.roomId] ?? data.roomId } } };
    });
    server.handle('room.leave', () => ({ ok: {} }));
    server.handle('share.stop', () => ({ ok: {} }));
    server.handle('subscribe.update', (data) => ({
      ok: { ignored: data.subs.map((s) => s.shareId).filter((id) => this.ignored.has(id)) },
    }));
  }

  /** Sends a room.state of roomId with the next rev. */
  sendState(roomId: string, overrides: Partial<RoomState> = {}): RoomState {
    const state = roomState(roomId, this.rev++, { shares: this.shares, ...overrides });
    this.#server.send('room.state', state);
    return state;
  }

  /** The roomIds of the room.join requests so far. */
  get joins(): string[] {
    return this.#server.messages('room.join').map((m) => (m.data as { roomId: string }).roomId);
  }

  /** The subs of each subscribe.update so far. */
  get subscribes(): SubscriptionWant[][] {
    return this.#server.messages('subscribe.update').map((m) => (m.data as SubscribeUpdate).subs);
  }

  /** The client's requests and notifications of one type. */
  sent(type: string): ClientMessage[] {
    return this.#server.messages(type);
  }
}

/** A local share that records what the session asks of it; bareShare() makes one without the ShareRecovery part. */
export class FakeShare implements ActiveShare, ShareRecovery {
  shareId: string;
  readonly kind = 'window' as const;
  readonly preview = null;
  state: LocalShareState = 'live';
  readonly params = null;
  readonly calls: string[] = [];
  readonly resyncs: ShareResyncContext[] = [];
  readonly #ended = new Set<(r: LocalEndReason) => void>();

  constructor(shareId: string) {
    this.shareId = shareId;
  }

  on(ev: 'state', fn: (s: LocalShareState) => void): () => void;
  on(ev: 'ended', fn: (r: LocalEndReason) => void): () => void;
  on(ev: 'hint', fn: (h: ShareHint | null) => void): () => void;
  on(
    ev: 'state' | 'ended' | 'hint',
    fn: ((s: LocalShareState) => void) | ((r: LocalEndReason) => void) | ((h: ShareHint | null) => void),
  ): () => void {
    if (ev !== 'ended') return () => undefined;
    const listener = fn as (r: LocalEndReason) => void;
    this.#ended.add(listener);
    return () => {
      this.#ended.delete(listener);
    };
  }

  /** The share ends by itself (the browser's "Stop sharing"). */
  end(reason: LocalEndReason): void {
    this.state = 'ended';
    for (const fn of [...this.#ended]) fn(reason);
  }

  setPreset(): Promise<void> {
    return Promise.resolve();
  }
  setPaused(): Promise<void> {
    return Promise.reject(new Error('not_supported'));
  }
  setAudioEnabled(): Promise<void> {
    return Promise.resolve();
  }
  stats(): Promise<null> {
    return Promise.resolve(null);
  }

  stop(): Promise<void> {
    this.calls.push('stop');
    this.end('user');
    return Promise.resolve();
  }

  resync(ctx: ShareResyncContext): Promise<void> {
    this.calls.push(ctx.kept ? 'resync:kept' : 'resync:lost');
    this.resyncs.push(ctx);
    return Promise.resolve();
  }

  republish(): Promise<void> {
    this.calls.push('republish');
    return Promise.resolve();
  }

  serverEnded(reason: EndReason | undefined): void {
    this.calls.push(`serverEnded:${reason ?? 'room.state'}`);
    this.state = 'ended';
  }
}

/** FakeShare without the ShareRecovery methods: what the session does with a provider that has none. */
export function bareShare(shareId: string): FakeShare {
  const share = new FakeShare(shareId);
  Object.assign(share, { resync: undefined, republish: undefined, serverEnded: undefined });
  return share;
}

/** A SharingProvider whose start() hands out the shares a test queued (FakeShare 's1', 's2', … by default). */
export class FakeSharing implements SharingProvider {
  readonly mode = 'in-page' as const;
  readonly starts: { opts: { preset: Preset; withAudio: boolean }; ctx: ShareContext }[] = [];
  /** What the next start() calls do: a share to resolve with, or an error to reject with. */
  readonly next: (FakeShare | Error)[] = [];
  #made = 0;

  pick(): Promise<PickedSource | null> {
    return Promise.resolve(null);
  }

  start(_src: PickedSource, opts: { preset: Preset; withAudio: boolean }, ctx: ShareContext): Promise<ActiveShare> {
    this.starts.push({ opts, ctx });
    const result = this.next.shift() ?? new FakeShare(`s${String(++this.#made)}`);
    return result instanceof Error ? Promise.reject(result) : Promise.resolve(result);
  }
}

/** A picked source for startShare(); the session passes it on without looking inside. */
export const pickedSource: PickedSource = {
  kind: 'window',
  audioScope: 'window',
  warning: null,
  preview: {} as MediaStream,
  release: () => undefined,
};

/** A sub PC that records the offers and candidates it got, and the deps the session made it with. */
export class FakeSubscriber implements SubscriberLike {
  readonly offers: PCOffer[] = [];
  readonly candidates: PCICE[] = [];
  closed = false;

  constructor(readonly deps: SubscriberDeps) {}

  handleOffer(o: PCOffer): Promise<void> {
    this.offers.push(o);
    return Promise.resolve();
  }
  handleIce(i: PCICE): Promise<void> {
    this.candidates.push(i);
    return Promise.resolve();
  }
  close(): void {
    this.closed = true;
  }
}

export interface Harness {
  readonly server: FakeSignalServer;
  readonly hub: FakeHub;
  readonly platform: Platform;
  readonly services: AppServices;
  readonly runtime: RoomRuntime;
  readonly sharing: FakeSharing;
  /** Every sub PC the session made, oldest first. */
  readonly subscribers: FakeSubscriber[];
  /** The messages of the toasts shown so far (uiStore keeps only the last four). */
  toasts(): string[];
  /** Starts the connection and waits for ready. */
  connect(): Promise<void>;
  /** Uninstalls the fake server and disposes the runtime. */
  close(): void;
}

export interface HarnessOptions {
  server?: FakeSignalServerOptions;
  platform?: Partial<Platform>;
  runtime?: RoomRuntimeOptions;
}

/** Installs a fake server and builds a runtime on it. Call vi.useFakeTimers() first, and close() in afterEach. */
export function createHarness(opts: HarnessOptions = {}): Harness {
  const server = FakeSignalServer.install(opts.server);
  const hub = new FakeHub(server);
  const sharing = new FakeSharing();
  const platform = createTestPlatform({ sharing, ...opts.platform });
  const services = createTestServices({ platform });
  const subscribers: FakeSubscriber[] = [];
  // The app's runtime of these services: what the hooks (useRoomRuntime) return too.
  const runtime = getRoomRuntime(services, {
    media: {
      createSubscriber: (deps) => {
        const sub = new FakeSubscriber(deps);
        subscribers.push(sub);
        return sub;
      },
    },
    ...opts.runtime,
  });
  const seen: string[] = [];
  const offToasts = services.ui.subscribe((s, prev) => {
    for (const t of s.toasts) {
      if (!prev.toasts.includes(t)) seen.push(t.message);
    }
  });
  return {
    server,
    hub,
    platform,
    services,
    runtime,
    sharing,
    subscribers,
    toasts: () => [...seen],
    async connect() {
      runtime.start();
      await tick();
      if (runtime.signal.state !== 'ready') throw new Error(`harness: the client is ${runtime.signal.state}`);
    },
    close() {
      offToasts();
      runtime.dispose();
      server.uninstall();
    },
  };
}

/**
 * The server is unreachable from now on: every new socket fails at once (connection refused). Returns the function
 * that brings it back.
 */
export function refuseConnections(server: FakeSignalServer): () => void {
  const connected = server.connected.bind(server);
  const autoOpen = server.autoOpen;
  server.autoOpen = false;
  server.connected = (socket) => {
    connected(socket);
    queueMicrotask(() => {
      socket.fail();
    });
  };
  return () => {
    server.autoOpen = autoOpen;
    server.connected = connected;
  };
}
