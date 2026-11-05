// "Open in the isshoni app" (docs/m2/03 §12.3) on the browser's room page: shown when user.connections lists an
// online desktop connection or /me/devices a desktop on this os, opening appLinkFor's link; "Nothing happened?"
// after 3 s unless the app took over; nothing for an http:// server.
import { act, fireEvent, screen } from '@testing-library/react';
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';

import type { DevicesResponse } from '../protocol/api.gen';
import type { OwnConnection } from '../protocol/types.gen';
import { appLikely, downloadPath, NOTHING_HAPPENED_AFTER_MS } from './OpenInApp';
import { createHarness, type Harness } from './testing/harness';
import { advance, PRELOAD_TIMEOUT_MS, preloadLazyChunks, renderRoomApp, stubRest } from './testing/page';

const ORIGIN = 'https://watch.example.com';
const desktopConn: OwnConnection = {
  id: 'c_desk',
  kind: 'desktop',
  role: 'viewer',
  status: 'online',
  os: 'windows',
  version: '0.2.0',
};
const device = (os: string): DevicesResponse['devices'][number] => ({
  id: 'd_1',
  name: 'Gaming PC',
  clientKind: 'desktop',
  os,
  current: false,
});
const devices = (os: string): DevicesResponse => ({ devices: [device(os)] });

let h: Harness;
let opened: string[];

beforeAll(async () => {
  await preloadLazyChunks();
}, PRELOAD_TIMEOUT_MS);

beforeEach(() => {
  vi.useFakeTimers();
  vi.spyOn(Math, 'random').mockReturnValue(0.5);
});

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

/** The browser's room page of the Lounge at `origin`, with /me/devices answering `list` (404 without). */
async function lounge(origin = ORIGIN, list?: DevicesResponse): Promise<void> {
  opened = [];
  h = createHarness({
    server: { welcome: { features: ['agent.relay', 'user.connections'] } },
    platform: {
      serverOrigin: origin,
      client: { kind: 'web', version: '0.2.0', os: 'windows', browser: 'chrome' },
      openExternal: (url) => {
        opened.push(url);
      },
    },
  });
  const fetch = stubRest(h.platform);
  if (list !== undefined) {
    const rest = fetch.getMockImplementation();
    fetch.mockImplementation((path, init) =>
      path === '/api/v1/me/devices'
        ? Promise.resolve(new Response(JSON.stringify(list), { headers: { 'Content-Type': 'application/json' } }))
        : (rest?.(path, init) ?? Promise.reject(new Error('no answer'))),
    );
  }
  renderRoomApp(h.services, '/r/lounge');
  await advance();
}

const entry = () => screen.queryByRole('button', { name: 'Open in the isshoni app' });

describe('appLikely and downloadPath', () => {
  it('counts an online desktop connection, or a desktop device on the same os', () => {
    expect(appLikely([desktopConn], undefined, 'macos')).toBe(true);
    expect(appLikely([{ ...desktopConn, status: 'reconnecting' }], undefined, 'windows')).toBe(false);
    expect(appLikely([{ ...desktopConn, kind: 'web' }], undefined, 'windows')).toBe(false);
    expect(appLikely(null, devices('windows'), 'windows')).toBe(true);
    expect(appLikely(null, devices('windows'), 'macos')).toBe(false);
    expect(appLikely(null, { devices: [{ ...device('windows'), clientKind: 'mobile' }] }, 'windows')).toBe(false);
    expect(appLikely(null, undefined, 'windows')).toBe(false);
  });

  it('adds the room to /download only when it follows the room-id rule', () => {
    expect(downloadPath('lounge')).toBe('/download?room=lounge');
    expect(downloadPath('a b')).toBe('/download');
  });
});

describe('the room page in the browser', () => {
  afterEach(() => {
    h.close();
  });

  it('shows the entry once user.connections lists the app online, and opens the room’s deep link', async () => {
    await lounge();
    expect(entry()).toBeNull();
    h.server.send('user.connections', { connections: [desktopConn] });
    await advance();
    fireEvent.click(screen.getByRole('button', { name: 'Open in the isshoni app' }));
    expect(opened).toEqual(['isshoni://watch.example.com/r/lounge']);
    expect(screen.queryByText(/Nothing happened\?/)).toBeNull();

    await advance(NOTHING_HAPPENED_AFTER_MS);
    expect(screen.getByText(/Nothing happened\?/)).toHaveTextContent('Nothing happened? Get the desktop app');
    expect(screen.getByRole('link', { name: 'Get the desktop app' })).toHaveAttribute('href', '/download?room=lounge');
  });

  it('says nothing more when the app took over (the tab was handed off)', async () => {
    await lounge();
    h.server.send('user.connections', { connections: [desktopConn] });
    await advance();
    fireEvent.click(screen.getByRole('button', { name: 'Open in the isshoni app' }));
    act(() => {
      h.runtime.viewer.store.getState().setHandedOff(true);
    });
    await advance(NOTHING_HAPPENED_AFTER_MS);
    expect(screen.queryByText(/Nothing happened\?/)).toBeNull();
  });

  it('shows it from /me/devices: a linked desktop on this os', async () => {
    await lounge(ORIGIN, devices('windows'));
    expect(entry()).not.toBeNull();
  });

  it('shows nothing for a device on another os, or without a deep link (an http:// server)', async () => {
    await lounge(ORIGIN, devices('macos'));
    expect(entry()).toBeNull();
    h.close();
    await lounge('http://watch.example.com', devices('windows'));
    h.server.send('user.connections', { connections: [desktopConn] });
    await advance();
    expect(entry()).toBeNull();
  });
});
