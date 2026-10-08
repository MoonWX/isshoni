// The one-time hint on iOS (05 §12.8): "Keep isshoni open while watching. Video pauses when you switch apps.",
// shown while a share is watched, until it is dismissed, which the device remembers.
import { act, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { PREFS_KEY } from '../app/prefs';
import type { ClientOS } from '../protocol/types.gen';
import { installFakeMedia, installFakeMediaElement, type FakeMediaElementControl } from '../test/fakeMedia';
import { createTestPlatform } from '../test/platform';
import { createTestServices, renderWithApp } from '../test/render';
import { IOS_HINT_ID } from './IosHint';
import { createViewer, syncRoom, type ViewerServices } from './services';
import { room, SELF, shareInfo } from './testing';
import { ViewerLayout } from './ViewerLayout';

const HINT = 'Keep isshoni open while watching. Video pauses when you switch apps.';
const BEA = shareInfo('s_bea', 'u_bea', 1);

let media: FakeMediaElementControl;
let viewer: ViewerServices;

beforeEach(() => {
  installFakeMedia();
  media = installFakeMediaElement();
  viewer = createViewer();
});

afterEach(() => {
  viewer.dispose();
  media.restore();
  vi.unstubAllGlobals();
});

/** The app's services on a device with this OS. */
function servicesOn(os: ClientOS, platform = createTestPlatform()) {
  return createTestServices({ platform: { ...platform, client: { ...platform.client, os } } });
}

describe('IosHint', () => {
  it('shows on iOS while a share is watched, and goes for good with "Got it"', async () => {
    const services = servicesOn('ios');
    syncRoom(viewer, room(BEA), SELF);
    const { unmount } = renderWithApp(<ViewerLayout viewer={viewer} />, { services });
    expect(screen.getByRole('note')).toHaveTextContent(HINT);

    await userEvent.click(screen.getByRole('button', { name: 'Got it' }));
    expect(screen.queryByText(HINT)).not.toBeInTheDocument();
    expect(services.prefs.getState().isDismissed(IOS_HINT_ID)).toBe(true);

    // The device remembers: a new page load on the same storage shows no hint.
    unmount();
    expect(services.platform.storage.local.get(PREFS_KEY)).toContain(IOS_HINT_ID);
    renderWithApp(<ViewerLayout viewer={viewer} />, { services: servicesOn('ios', services.platform) });
    expect(screen.queryByText(HINT)).not.toBeInTheDocument();
  });

  it('waits until there is something to watch', () => {
    renderWithApp(<ViewerLayout viewer={viewer} />, { services: servicesOn('ios') });
    expect(screen.queryByText(HINT)).not.toBeInTheDocument();
    act(() => {
      syncRoom(viewer, room(shareInfo('s_mine', 'u_alex', 1, { connectionId: 'c_me' })), SELF);
    });
    expect(screen.queryByText(HINT)).not.toBeInTheDocument(); // only the own preview
    act(() => {
      syncRoom(viewer, room(BEA), SELF);
    });
    expect(screen.getByText(HINT)).toBeInTheDocument();
  });

  it('is out of the way in fullscreen', () => {
    syncRoom(viewer, room(BEA), SELF);
    renderWithApp(<ViewerLayout viewer={viewer} />, { services: servicesOn('ios') });
    act(() => {
      viewer.store.getState().setFullscreen(true);
    });
    expect(screen.queryByText(HINT)).not.toBeInTheDocument();
    act(() => {
      viewer.store.getState().setFullscreen(false);
    });
    expect(screen.getByText(HINT)).toBeInTheDocument();
  });

  it('waits while the media banner has something more urgent to say', () => {
    syncRoom(viewer, room(BEA), SELF);
    renderWithApp(<ViewerLayout viewer={viewer} />, { services: servicesOn('ios') });
    act(() => {
      viewer.store.getState().setMedia('unreachable');
    });
    expect(screen.getByText(/media port/)).toBeInTheDocument();
    expect(screen.queryByText(HINT)).not.toBeInTheDocument();
    act(() => {
      viewer.store.getState().setMedia('connected');
    });
    expect(screen.getByText(HINT)).toBeInTheDocument();
  });

  it.each<ClientOS>(['android', 'macos', 'windows', 'linux', 'other'])('never shows on %s', (os) => {
    syncRoom(viewer, room(BEA), SELF);
    renderWithApp(<ViewerLayout viewer={viewer} />, { services: servicesOn(os) });
    expect(screen.queryByText(HINT)).not.toBeInTheDocument();
  });

  it('never shows outside the app’s providers', () => {
    syncRoom(viewer, room(BEA), SELF);
    render(<ViewerLayout viewer={viewer} />);
    expect(screen.queryByText(HINT)).not.toBeInTheDocument();
  });
});
