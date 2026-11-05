// The desktop app's parts of the app shell (docs/m2/03 §16.2, §16.3; README WA82): VersionMismatch's desktop actions
// (Update the app and Open in browser, which opens the current room) on the app's own DesktopPlatform over the fake
// bridge, the browser's screen unchanged, and the banner of a required update above every page in the desktop only.
import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { createMemoryRouter } from 'react-router';
import { afterEach, describe, expect, it } from 'vitest';

import { BrowserPlatform } from '../platform/browser/BrowserPlatform';
import { BridgeError } from '../platform/desktop/errors';
import { authState, updateStatus } from '../platform/desktop/testing/fakeBridge';
import { createTestPlatform, testOrigin } from '../test/platform';
import { createTestServices } from '../test/render';
import { App, AppProviders } from './App';
import { AppScreenView } from './screens';
import { createDesktopTestPlatform } from './testing/desktop';
import type { AppScreen } from './uiStore';

const MISMATCH: AppScreen = { kind: 'versionMismatch', serverVersion: '0.3.0' };

/** The version screen on the app's DesktopPlatform (its real versionActions) over the fake bridge. */
function renderDesktopScreen(screenValue: AppScreen = MISMATCH) {
  const d = createDesktopTestPlatform({ real: true });
  const services = createTestServices({ platform: d.platform });
  render(
    <AppProviders services={services}>
      <AppScreenView screen={screenValue} platform={d.platform} />
    </AppProviders>,
  );
  return d;
}

afterEach(() => {
  history.replaceState(null, '', '/');
});

describe('VersionMismatch in the desktop app (03 §16.3)', () => {
  it('update_app: the versions, Update the app and Open in browser; no Reload', async () => {
    renderDesktopScreen();
    expect(
      await screen.findByText('This server needs a newer isshoni app. You have 0.2.0; the server runs 0.3.0.'),
    ).toBeInTheDocument();
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('This server needs a newer isshoni app');
    expect(screen.getByRole('button', { name: 'Update the app' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Open in browser' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Try again' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Reload' })).not.toBeInTheDocument();
  });

  it('Open in browser opens the current room on the server in the system browser', async () => {
    const user = userEvent.setup();
    history.replaceState(null, '', '/r/lounge');
    const d = renderDesktopScreen();
    await user.click(await screen.findByRole('button', { name: 'Open in browser' }));
    expect(d.fake.callsOf('app.openExternal')).toEqual([[`${testOrigin()}/r/lounge`]]);
  });

  it('update_server: only Open in browser, and the admin text', async () => {
    const user = userEvent.setup();
    const d = renderDesktopScreen({ kind: 'versionMismatch', serverVersion: '0.1.0', serverOlder: true });
    expect(
      await screen.findByText(
        'This server runs isshoni 0.1.0, which is too old for this app. Ask your admin to update it.',
      ),
    ).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Update the app' })).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Open in browser' }));
    expect(d.fake.callsOf('app.openExternal')).toEqual([[`${testOrigin()}/`]]);
  });

  it('Update the app installs a ready update', async () => {
    const user = userEvent.setup();
    const d = renderDesktopScreen();
    d.fake.answers['updates.check'] = updateStatus('ready', { available: '0.3.0' });
    await user.click(await screen.findByRole('button', { name: 'Update the app' }));
    expect(await screen.findByText('isshoni restarts to update.')).toBeInTheDocument();
    expect(d.fake.callsOf('updates.install')).toEqual([[]]);
    expect(screen.queryByRole('button', { name: "Get it from your server's Download page" })).not.toBeInTheDocument();
  });

  it("Update the app when none is newer: says so, with the server's Download page", async () => {
    const user = userEvent.setup();
    const d = renderDesktopScreen();
    d.fake.answers['updates.check'] = updateStatus('up_to_date');
    await user.click(await screen.findByRole('button', { name: 'Update the app' }));
    expect(await screen.findByText('No newer version is available yet')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: "Get it from your server's Download page" }));
    expect(d.fake.callsOf('app.openExternal')).toEqual([[`${testOrigin()}/download`]]);
  });

  it.each<[string, () => Promise<unknown>, string]>([
    [
      'manifest_expired',
      () => Promise.resolve(updateStatus('error', { errorCode: 'manifest_expired' })),
      "Update info is out of date; get the new version from your server's Download page",
    ],
    ['off', () => Promise.resolve(updateStatus('off')), 'Turned off'],
    [
      'disabled',
      () => Promise.resolve(updateStatus('disabled', { errorCode: 'not_release' })),
      'Updates are off in this build',
    ],
    ['busy', () => Promise.reject(new BridgeError('busy')), 'Stop sharing first.'],
    [
      'update.network',
      () => Promise.reject(new BridgeError('update.network')),
      "isshoni couldn't reach GitHub to check for updates. Check your connection.",
    ],
  ])('Update the app: %s has its text', async (_name, answer, text) => {
    const user = userEvent.setup();
    const d = renderDesktopScreen();
    d.fake.answers['updates.check'] = answer;
    await user.click(await screen.findByRole('button', { name: 'Update the app' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(text);
    expect(screen.getByRole('button', { name: "Get it from your server's Download page" })).toBeInTheDocument();
  });

  it('a build that cannot update itself opens its release notes', async () => {
    const user = userEvent.setup();
    const d = renderDesktopScreen();
    const notesUrl = 'https://github.com/MoonWX/isshoni/releases/tag/v0.3.0';
    d.fake.answers['updates.check'] = updateStatus('error', { errorCode: 'not_installed', notesUrl });
    await user.click(await screen.findByRole('button', { name: 'Update the app' }));
    expect(await screen.findByText(/This copy of isshoni can't update itself/)).toBeInTheDocument();
    expect(d.fake.callsOf('app.openExternal')).toEqual([[notesUrl]]);
  });

  it('Try again asks Auth.Recheck()', async () => {
    const user = userEvent.setup();
    const d = renderDesktopScreen();
    d.fake.answers['auth.recheck'] = {
      ...authState(testOrigin(), 'incompatible'),
      compat: {
        verdict: 'update_app',
        reason: 'min_client_version',
        serverVersion: '0.3.0',
        minClientVersion: '0.3.0',
      },
    };
    await user.click(await screen.findByRole('button', { name: 'Try again' }));
    await waitFor(() => {
      expect(d.fake.callsOf('auth.recheck')).toEqual([[]]);
    });
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('This server needs a newer isshoni app');
  });
});

describe('VersionMismatch in the browser is unchanged', () => {
  it('BrowserPlatform offers Reload only', async () => {
    const user = userEvent.setup();
    const platform = new BrowserPlatform();
    const actions = platform.versionActions();
    expect(Object.keys(actions)).toEqual(['reload']);
    let reloaded = 0;
    render(
      <AppProviders services={createTestServices()}>
        <AppScreenView
          screen={MISMATCH}
          platform={createTestPlatform({
            versionActions: () => ({
              reload: () => {
                reloaded += 1;
              },
            }),
          })}
        />
      </AppProviders>,
    );
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('This page is out of date');
    expect(screen.queryByRole('button', { name: 'Update the app' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Open in browser' })).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Reload' }));
    expect(reloaded).toBe(1);
  });
});

describe('the required update banner in the shell (03 §16.2)', () => {
  const router = () => createMemoryRouter([{ path: '*', element: <h1>page</h1> }]);

  it('shows above every page in the desktop app, and above an app-level screen', async () => {
    const d = createDesktopTestPlatform();
    d.fake.answers['updates.status'] = updateStatus('ready', { available: '0.3.0', required: true });
    d.fake.answers['share.current'] = null;
    const services = createTestServices({ platform: d.platform });
    render(<App services={services} router={router()} />);
    const banner = await screen.findByRole('region', { name: 'Important update' });
    expect(within(banner).getByRole('button', { name: 'Restart and update' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'page' })).toBeInTheDocument();
    act(() => {
      services.ui.getState().showScreen({ kind: 'fatal' });
    });
    expect(screen.getByRole('region', { name: 'Important update' })).toBeInTheDocument();
  });

  it('is not there in the browser', async () => {
    const services = createTestServices();
    render(<App services={services} router={router()} />);
    expect(await screen.findByRole('heading', { name: 'page' })).toBeInTheDocument();
    expect(screen.queryByRole('region', { name: 'Important update' })).not.toBeInTheDocument();
  });
});
