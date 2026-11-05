// The web → desktop handoff (docs/m2/03 §12.2; handoff.ts) on a room runtime against the fake signaling server:
// user.connections, the tabs' playback.state and their receiver of playback.stop, the desktop page's playback.stop
// (once per join, never for a resync's re-join) and its App.OwnConnections, and the endpoint mute that the runtime
// now attaches (README §3 item 87).
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { createDesktopTestPlatform, type DesktopTestPlatform } from '../app/testing/desktop';
import { BackoffMaxMs } from '../protocol/signal-client';
import { makeError } from '../protocol/testing';
import type { AgentSend, OwnConnection, ParticipantInfo } from '../protocol/types.gen';
import { queryKeys } from '../protocol/queryKeys';
import {
  browserFamily,
  fromOwnDesktop,
  HANDOFF_VERIFY_MS,
  onlineDesktops,
  PLAYBACK_STATE_REPEAT_MS,
  playsAudibly,
  stopForRoom,
} from './handoff';
import { SUBSCRIBE_DEBOUNCE_MS } from './subscriptionSync';
import { createHarness, participant, shareInfo, tick, type Harness, type HarnessOptions } from './testing/harness';

/** The signed-in user of the fake server's welcome. */
const ME = 'k3m9p2qxw7ht';
const RELAY = { welcome: { features: ['agent.relay' as const, 'user.connections' as const] } };

function own(id: string, kind: OwnConnection['kind'], over: Partial<OwnConnection> = {}): OwnConnection {
  return {
    id,
    kind,
    role: kind === 'desktop' ? 'viewer' : 'full',
    status: 'online',
    os: 'windows',
    version: '0.2.0',
    ...over,
  };
}

/** The user's participant entry with a desktop connection c_desk besides the tab. */
function meWithDesktop(desktopId = 'c_desk'): ParticipantInfo {
  return participant(ME, 'alex', {
    connections: [
      { id: 'c_tab', kind: 'web', role: 'full', status: 'online' },
      { id: desktopId, kind: 'desktop', role: 'viewer', status: 'online' },
    ],
  });
}

const boShare = shareInfo('s_bo', 'u_bo', 'c_bo');
const people = [participant(ME, 'alex'), participant('u_bo', 'bo')];

beforeEach(() => {
  vi.useFakeTimers();
  vi.spyOn(Math, 'random').mockReturnValue(0.5);
});

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe('helpers', () => {
  it('maps client.browser to the four families: safari and anything else are other', () => {
    expect(['chrome', 'edge', 'firefox', 'safari', 'other', undefined, 'opera'].map(browserFamily)).toEqual([
      'chrome',
      'edge',
      'firefox',
      'other',
      'other',
      'other',
      'other',
    ]);
  });

  it('takes a playback.stop only with v 1 and the session’s room', () => {
    expect(stopForRoom({ v: 1, roomId: 'lounge' }, 'lounge')).toBe(true);
    expect(stopForRoom({ v: 1, roomId: 'lounge', later: true }, 'lounge')).toBe(true);
    expect(stopForRoom({ v: 2, roomId: 'lounge' }, 'lounge')).toBe(false);
    expect(stopForRoom({ v: 1, roomId: 'games' }, 'lounge')).toBe(false);
    expect(stopForRoom({ v: 1, roomId: 'lounge' }, null)).toBe(false);
    expect(stopForRoom({ roomId: 'lounge' }, 'lounge')).toBe(false);
    expect(stopForRoom('lounge', 'lounge')).toBe(false);
    expect(stopForRoom(null, 'lounge')).toBe(false);
    expect(stopForRoom([1], 'lounge')).toBe(false);
  });

  it('knows a sender only as a desktop connection of the user’s own participant entry', () => {
    const state = { roomId: 'lounge', rev: 1, participants: [meWithDesktop(), participant('u_bo', 'bo')], shares: [] };
    expect(fromOwnDesktop(state, ME, 'c_desk')).toBe(true);
    expect(fromOwnDesktop(state, ME, 'c_tab')).toBe(false); // the user's, but a web tab
    expect(fromOwnDesktop(state, 'u_bo', 'c_desk')).toBe(false);
    expect(fromOwnDesktop(state, ME, 'c_u_bo')).toBe(false); // another user's
    expect(fromOwnDesktop(null, ME, 'c_desk')).toBe(false);
  });

  it('lists the online desktop connections: unknown statuses count as online', () => {
    const list = [
      own('c_1', 'desktop'),
      own('c_2', 'desktop', { status: 'reconnecting' }),
      own('c_3', 'web'),
      own('c_4', 'desktop', { status: 'later' as OwnConnection['status'] }),
    ];
    expect(onlineDesktops(list).map((c) => c.id)).toEqual(['c_1', 'c_4']);
    expect(onlineDesktops(null)).toEqual([]);
  });
});

describe('a browser tab', () => {
  let h: Harness;
  const viewer = () => h.runtime.viewer.store.getState();
  const sends = (): AgentSend[] => h.hub.sent('agent.send').map((m) => m.data as AgentSend);
  const states = () => sends().filter((s) => s.kind === 'playback.state');

  /** A Chrome tab on macOS (the report's browser and os). */
  const client = { kind: 'web' as const, version: '0.2.0', os: 'macos' as const, browser: 'chrome' };

  async function inLounge(opts: HarnessOptions = { server: RELAY }): Promise<void> {
    h = createHarness({ ...opts, platform: { client, ...opts.platform } });
    h.server.handle('agent.send', () => ({ ok: { delivered: 1 } }));
    const joined = h.runtime.session.join('lounge');
    await h.connect();
    await joined;
  }

  /** bo's share with sound on the stage, and the element plays: the tab is audible. */
  async function playing(): Promise<void> {
    h.hub.sendState('lounge', { participants: [meWithDesktop(), participant('u_bo', 'bo')], shares: [boShare] });
    await tick();
    viewer().setAudio('playing');
    await tick(SUBSCRIBE_DEBOUNCE_MS);
    expect(playsAudibly(viewer())).toBe(true);
  }

  afterEach(() => {
    h.close();
  });

  it('keeps user.connections, and forgets them when the connection stops', async () => {
    await inLounge();
    expect(h.runtime.handoff.store.getState().connections).toBeNull();
    const list = [own('c_desk', 'desktop'), own('c_tab', 'web')];
    h.server.send('user.connections', { connections: list });
    await tick();
    expect(h.runtime.handoff.store.getState().connections).toEqual(list);
    h.runtime.stop();
    await tick();
    expect(h.runtime.handoff.store.getState().connections).toBeNull();
  });

  it('sends playback.state to each online desktop connection when its playback starts and stops', async () => {
    await inLounge();
    h.server.send('user.connections', {
      connections: [
        own('c_c1', 'desktop'),
        own('c_c2', 'desktop', { role: 'publisher' }),
        own('c_gone', 'desktop', { status: 'reconnecting' }),
        own('c_phone', 'web', { role: 'viewer', os: 'android' }),
      ],
    });
    await tick();
    // Nothing plays yet: the app hears that too, once.
    expect(states()).toEqual([
      { to: 'c_c1', kind: 'playback.state', payload: { playing: false, browser: 'chrome', os: 'macos' } },
      { to: 'c_c2', kind: 'playback.state', payload: { playing: false, browser: 'chrome', os: 'macos' } },
    ]);

    await playing();
    expect(states().slice(2)).toEqual([
      { to: 'c_c1', kind: 'playback.state', payload: { playing: true, browser: 'chrome', os: 'macos' } },
      { to: 'c_c2', kind: 'playback.state', payload: { playing: true, browser: 'chrome', os: 'macos' } },
    ]);
    // Never with toRole (01 §11.11).
    expect(states().every((s) => s.toRole === undefined)).toBe(true);

    // A desktop that appears gets the value; the others aren't told again.
    h.server.send('user.connections', {
      connections: [own('c_c1', 'desktop'), own('c_c2', 'desktop'), own('c_c3', 'desktop')],
    });
    await tick();
    expect(states().slice(4)).toEqual([
      { to: 'c_c3', kind: 'playback.state', payload: { playing: true, browser: 'chrome', os: 'macos' } },
    ]);

    // Muted by the user: no audible playback.
    viewer().setAudio('muted');
    await tick();
    expect(
      states()
        .slice(5)
        .map((s) => [s.to, (s.payload as { playing: boolean }).playing]),
    ).toEqual([
      ['c_c1', false],
      ['c_c2', false],
      ['c_c3', false],
    ]);
  });

  it('tells a desktop connection that appeared once more after PLAYBACK_STATE_REPEAT_MS', async () => {
    await inLounge();
    await playing();
    h.server.send('user.connections', { connections: [own('c_c2', 'desktop')] });
    await tick();
    expect(states().map((s) => s.to)).toEqual(['c_c2']);
    await tick(PLAYBACK_STATE_REPEAT_MS);
    expect(states()).toHaveLength(2);
    expect(states()[1]).toEqual(states()[0]);
    // Only for one that appeared: an unchanged list repeats nothing.
    h.server.send('user.connections', { connections: [own('c_c2', 'desktop')] });
    await tick(PLAYBACK_STATE_REPEAT_MS);
    expect(states()).toHaveLength(2);
  });

  it('tells the desktops again after a new welcome, and never without agent.relay', async () => {
    await inLounge();
    h.server.send('user.connections', { connections: [own('c_c1', 'desktop')] });
    await tick();
    expect(states()).toHaveLength(1);
    await tick(PLAYBACK_STATE_REPEAT_MS);
    expect(states()).toHaveLength(2); // the repeat for a desktop that appeared
    h.server.restart();
    h.server.drop();
    await tick(BackoffMaxMs);
    expect(h.runtime.signal.state).toBe('ready');
    expect(states()).toHaveLength(3);

    h.close();
    await inLounge({ server: {} });
    h.server.send('user.connections', { connections: [own('c_c1', 'desktop')] });
    await tick();
    await playing();
    expect(states()).toEqual([]);
  });

  it('ignores a failed playback.state (no tab there, or anything else)', async () => {
    await inLounge();
    h.server.handle('agent.send', () => ({ error: makeError('agent_target_not_found', 'request') }));
    h.server.send('user.connections', { connections: [own('c_c1', 'desktop')] });
    await tick();
    await playing();
    expect(states()).toHaveLength(2);
    expect(h.runtime.signal.state).toBe('ready');
  });

  it('hands off on playback.stop from its own desktop connection: nothing subscribed, "Watch here instead" resumes', async () => {
    await inLounge();
    h.server.send('user.connections', { connections: [own('c_desk', 'desktop')] });
    await playing();
    expect(h.hub.subscribes.at(-1)).toEqual([{ shareId: 's_bo', video: 'off', audio: 'on' }]);

    h.server.send('agent.recv', { from: 'c_desk', kind: 'playback.stop', payload: { v: 1, roomId: 'lounge' } });
    await tick(SUBSCRIBE_DEBOUNCE_MS);
    expect(viewer().handedOff).toBe(true);
    expect(h.hub.subscribes.at(-1)).toEqual([{ shareId: 's_bo', video: 'off', audio: 'off' }]);
    // The app hears that this tab stopped playing.
    expect(states().at(-1)).toMatchObject({ to: 'c_desk', payload: { playing: false } });

    viewer().setHandedOff(false);
    await tick(SUBSCRIBE_DEBOUNCE_MS);
    expect(h.hub.subscribes.at(-1)).toEqual([{ shareId: 's_bo', video: 'off', audio: 'on' }]);
    expect(states().at(-1)).toMatchObject({ to: 'c_desk', payload: { playing: true } });
  });

  it('ignores another v, another room, a web connection and another user’s desktop', async () => {
    await inLounge();
    await playing();
    const people2 = [
      meWithDesktop(),
      participant('u_bo', 'bo', {
        connections: [{ id: 'c_bodesk', kind: 'desktop', role: 'viewer', status: 'online' }],
      }),
    ];
    h.hub.sendState('lounge', { participants: people2, shares: [boShare] });
    await tick();
    for (const [from, payload] of [
      ['c_desk', { v: 2, roomId: 'lounge' }],
      ['c_desk', { v: 1, roomId: 'games' }],
      ['c_tab', { v: 1, roomId: 'lounge' }],
      ['c_bodesk', { v: 1, roomId: 'lounge' }],
    ] as const) {
      h.server.send('agent.recv', { from, kind: 'playback.stop', payload });
    }
    h.server.send('agent.recv', { from: 'c_desk', kind: 'share.request', payload: { v: 1, roomId: 'lounge' } });
    await tick(HANDOFF_VERIFY_MS + 1);
    h.hub.sendState('lounge', { participants: people2, shares: [boShare] });
    await tick();
    expect(viewer().handedOff).toBe(false);
  });

  it('waits a little for a room.state that lists the sender, and the state ends with a room switch', async () => {
    await inLounge();
    h.hub.sendState('lounge', { participants: people, shares: [boShare] });
    await tick();
    h.server.send('agent.recv', { from: 'c_desk', kind: 'playback.stop', payload: { v: 1, roomId: 'lounge' } });
    await tick();
    expect(viewer().handedOff).toBe(false);
    h.hub.sendState('lounge', { participants: [meWithDesktop(), participant('u_bo', 'bo')], shares: [boShare] });
    await tick();
    expect(viewer().handedOff).toBe(true);

    await h.runtime.session.join('games');
    await tick();
    expect(viewer().handedOff).toBe(false);

    // Too late: the snapshot that lists the sender came after HANDOFF_VERIFY_MS.
    h.server.send('agent.recv', { from: 'c_desk', kind: 'playback.stop', payload: { v: 1, roomId: 'games' } });
    await tick(HANDOFF_VERIFY_MS + 1);
    h.hub.sendState('games', { participants: [meWithDesktop()] });
    await tick();
    expect(viewer().handedOff).toBe(false);
  });

  it('never sends playback.stop', async () => {
    await inLounge();
    await h.runtime.session.join('games');
    await tick();
    h.runtime.handoff.requestStop();
    await tick();
    expect(sends().filter((s) => s.kind === 'playback.stop')).toEqual([]);
  });
});

describe('the desktop app’s page (C1)', () => {
  let h: Harness;
  let d: DesktopTestPlatform;
  const stops = (): AgentSend[] =>
    h.hub
      .sent('agent.send')
      .map((m) => m.data as AgentSend)
      .filter((s) => s.kind === 'playback.stop');

  function desktopPage(): void {
    d = createDesktopTestPlatform();
    const { kind, client, role } = d.platform;
    h = createHarness({
      server: RELAY,
      platform: { kind, client, role, versionActions: () => ({}), desktop: d.platform.desktop ?? null },
    });
    h.services.queryClient.setQueryData(queryKeys.me, { user: { id: ME } });
    h.server.handle('agent.send', () => ({ error: makeError('agent_target_not_found', 'request') }));
  }

  afterEach(() => {
    h.close();
  });

  it('sends playback.stop with toRole full once its join’s room.state is there, and not for a resync', async () => {
    desktopPage();
    const joined = h.runtime.session.join('lounge');
    await h.connect();
    await joined;
    await tick();
    expect(stops()).toEqual([{ toRole: 'full', kind: 'playback.stop', payload: { v: 1, roomId: 'lounge' } }]);

    // More snapshots of the room, a resumed welcome, a new connection's re-join: none of them.
    h.hub.sendState('lounge');
    h.server.drop();
    await tick(BackoffMaxMs);
    h.server.restart();
    h.server.drop();
    await tick(BackoffMaxMs);
    // Each welcome re-joined (the fake's resumed welcome names no room): resyncs, not joins the user asked for.
    expect(h.hub.joins).toEqual(['lounge', 'lounge', 'lounge']);
    expect(stops()).toHaveLength(1);

    // The room page again for the room it is in: nothing; another room: one more.
    await h.runtime.session.join('lounge');
    await h.runtime.session.join('games');
    await tick();
    expect(stops().map((s) => (s.payload as { roomId: string }).roomId)).toEqual(['lounge', 'games']);

    // A deep link to the room it is in (03 §13.2).
    h.runtime.handoff.requestStop();
    await tick();
    expect(stops()).toHaveLength(3);
  });

  it('sends no playback.stop without agent.relay, and no playback.state ever', async () => {
    d = createDesktopTestPlatform();
    const { kind, client, role } = d.platform;
    h = createHarness({
      platform: { kind, client, role, versionActions: () => ({}), desktop: d.platform.desktop ?? null },
    });
    const joined = h.runtime.session.join('lounge');
    await h.connect();
    await joined;
    h.hub.sendState('lounge', { participants: people, shares: [boShare] });
    h.runtime.viewer.store.getState().setAudio('playing');
    h.server.send('user.connections', { connections: [own('c_c2', 'desktop')] });
    await tick();
    expect(h.hub.sent('agent.send')).toEqual([]);
  });

  it('gives Go the online ids of user.connections on every change (App.OwnConnections)', async () => {
    desktopPage();
    await h.connect();
    h.server.send('user.connections', { connections: [own('c_1', 'desktop'), own('c_tab', 'web')] });
    h.server.send('user.connections', { connections: [own('c_1', 'desktop'), own('c_tab', 'web')] });
    h.server.send('user.connections', {
      connections: [own('c_1', 'desktop'), own('c_tab', 'web', { status: 'reconnecting' })],
    });
    await tick();
    expect(d.fake.callsOf('app.ownConnections')).toEqual([[['c_1', 'c_tab']], [['c_1']]]);
  });

  it('mutes the viewer while the share uses the endpoint engine, on every page (the runtime attaches it)', async () => {
    desktopPage();
    await h.connect();
    const store = h.runtime.viewer.store;
    d.fake.emit('audio.report', { seq: 1, live: true, engine: 'endpoint' } as never);
    expect(store.getState().audioMutedForShare).toBe(true);
    d.fake.emit('audio.report', { seq: 2, live: false, engine: 'endpoint' } as never);
    expect(store.getState().audioMutedForShare).toBe(false);
  });
});
