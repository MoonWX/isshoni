// The auth and setup folders as the app loads them: through the router's own import.meta.glob (app/router.tsx's
// page-folder contract), each page by its export name. Plus /about, which has no form.
import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { createAppRoutes, defaultSources } from '../app/router';
import { infoFixture, server, signedIn } from '../test/msw';
import { createTestPlatform } from '../test/platform';
import { createTestServices, renderRoute } from '../test/render';
import { CODE_SIGNING_URL, PRIVACY_URL, PROJECT_SITE_URL } from './AboutPage';
import * as authFolder from './index';
import { mockSession, renderPage } from './testing/harness';

describe('the page folders (app/router.tsx)', () => {
  it('auth/index.ts exports exactly the pages the router asks for', () => {
    expect(Object.keys(authFolder).sort()).toEqual([
      'AboutPage',
      'InvitePage',
      'LoginPage',
      'PendingPage',
      'ResetPage',
      'SignupPage',
    ]);
  });

  it('the build finds both folders, and setup/ has SetupPage', async () => {
    const { lazy } = defaultSources();
    expect(lazy.auth).toBeTypeOf('function');
    expect(lazy.setup).toBeTypeOf('function');
    expect(Object.keys((await lazy.auth?.()) ?? {}).sort()).toEqual(Object.keys(authFolder).sort());
    expect(Object.keys((await lazy.setup?.()) ?? {})).toContain('SetupPage');
  });

  it.each([
    ['/login', 'Log in to Test server'],
    ['/pending', 'Request sent'],
    ['/about', 'About isshoni'],
    ['/invite', 'This invite link is incomplete'],
    ['/reset', 'This reset link is incomplete'],
    ['/setup', 'This server is already set up'],
  ])('%s renders its page through the default routes', async (path, heading) => {
    server.use(signedIn(null));
    renderRoute({ path, routes: createAppRoutes() });
    expect(await screen.findByRole('heading', { level: 1, name: heading })).toBeInTheDocument();
  });

  it('/signup renders its page in approval mode', async () => {
    server.use(signedIn(null));
    const services = createTestServices({ info: infoFixture({ registration: 'approval' }) });
    renderRoute({ path: '/signup', routes: createAppRoutes(), services });
    expect(await screen.findByRole('heading', { level: 1, name: 'Request an account' })).toBeInTheDocument();
  });

  it('a guarded route without a session ends on the real login page', async () => {
    server.use(signedIn(null));
    const { router } = renderRoute({ path: '/account/devices', routes: createAppRoutes() });
    expect(await screen.findByRole('heading', { name: 'Log in to Test server' })).toBeInTheDocument();
    expect(router.state.location.search).toBe(`?next=${encodeURIComponent('/account/devices')}`);
  });
});

describe('AboutPage', () => {
  it('shows both versions, the trust model and the links, without asking who is signed in', async () => {
    const session = mockSession();
    renderPage({ path: '/about' });
    expect(await screen.findByRole('heading', { level: 1, name: 'About isshoni' })).toBeInTheDocument();

    const versions = screen.getByRole('region', { name: 'Versions' });
    expect(within(versions).getByText('Test server')).toBeInTheDocument();
    expect(within(versions).getByText('0.0.0-test')).toBeInTheDocument();
    expect(within(versions).getByText(__ISSHONI_VERSION__)).toBeInTheDocument();

    const trust = within(screen.getByRole('region', { name: 'Privacy and trust' })).getAllByRole('listitem');
    expect(trust).toHaveLength(5);
    expect(trust[0]).toHaveTextContent(/The server is trusted.*whoever runs it could see the streams/);
    expect(trust[1]).toHaveTextContent(/no recording.*no hidden viewers/);
    expect(trust[4]).toHaveTextContent(/^No telemetry/);

    const links = within(screen.getByRole('region', { name: 'More' }));
    const hrefs = Object.fromEntries(
      links.getAllByRole('link').map((a) => [a.textContent, a.getAttribute('href')] as const),
    );
    expect(hrefs).toEqual({
      'Privacy, in full': PRIVACY_URL,
      'Code-signing policy': CODE_SIGNING_URL,
      'Open-source licenses': `${window.location.origin}/licenses.txt`,
      'Project site': PROJECT_SITE_URL,
    });
    expect(PROJECT_SITE_URL).toBe('https://moonwx.github.io/isshoni/');
    expect(PRIVACY_URL).toBe('https://moonwx.github.io/isshoni/privacy');
    expect(CODE_SIGNING_URL).toBe('https://moonwx.github.io/isshoni/code-signing');
    // External links open a new tab without opener or referrer (05 §20).
    for (const a of links.getAllByRole('link')) {
      expect(a).toHaveAttribute('target', '_blank');
      expect(a).toHaveAttribute('rel', 'noopener noreferrer');
    }
    expect(screen.getByRole('link', { name: 'Back to isshoni' })).toHaveAttribute('href', '/');
    expect(session.meCalls).toBe(0);
  });

  it('outside a browser tab the platform opens external links', async () => {
    mockSession();
    const openExternal = vi.fn();
    renderPage({ path: '/about', platform: createTestPlatform({ kind: 'desktop', openExternal }) });
    await userEvent.click(await screen.findByRole('link', { name: 'Privacy, in full' }));
    expect(openExternal).toHaveBeenCalledExactlyOnceWith(PRIVACY_URL);
  });
});
