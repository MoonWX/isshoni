// The account folder's contract with the router (05 §5 "Page folders"): its pages are named exports of index.ts,
// which app/router.tsx finds by itself. These run the app's own routes with the build's real folders, as boot does.
import { screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { defaultSources } from '../app/router';
import { server, signedIn } from '../test/msw';
import { renderRoute } from '../test/render';
import * as folder from './index';
import { mockDevices, mockSessions, sessionFixture } from './testing/harness';

describe('account/index.ts', () => {
  it('exports the pages by the names the router asks for', async () => {
    expect(Object.keys(folder).sort()).toEqual(['AccountPage', 'DevicesPage']);
    const load = defaultSources().lazy.account;
    expect(load).toBeTypeOf('function');
    expect(await load?.()).toMatchObject({ AccountPage: folder.AccountPage, DevicesPage: folder.DevicesPage });
  });

  it('/account is the account page for a signed-in user', async () => {
    server.use(signedIn());
    renderRoute({ path: '/account' });
    expect(await screen.findByRole('heading', { level: 1, name: 'Your account' })).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: 'Not in this build yet' })).not.toBeInTheDocument();
  });

  it('/account/devices is the devices page', async () => {
    server.use(signedIn());
    mockSessions([sessionFixture({ current: true })]);
    mockDevices();
    renderRoute({ path: '/account/devices' });
    expect(await screen.findByRole('heading', { level: 1, name: 'Devices' })).toBeInTheDocument();
    expect(await screen.findByText('This browser')).toBeInTheDocument();
  });

  it('/account sends a signed-out visitor to the login page and back', async () => {
    server.use(signedIn(null));
    const { router } = renderRoute({ path: '/account' });
    expect(await screen.findByRole('heading', { name: 'Log in to Test server' })).toBeInTheDocument();
    expect(router.state.location.pathname + router.state.location.search).toBe('/login?next=%2Faccount');
  });

  it('/account/notifications is the router stand-in until the notifications page exists', async () => {
    // AccountShell already links there (05 §16.3 "Account → Notifications"). When NotificationsPage is exported
    // from index.ts this test goes with the stand-in.
    server.use(signedIn());
    renderRoute({ path: '/account/notifications' });
    expect(await screen.findByRole('heading', { name: 'Not in this build yet' })).toBeInTheDocument();
  });
});
