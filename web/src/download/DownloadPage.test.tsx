// /download (05 §5, §15.4): the M1 placeholder, through the app's own routes. The router finds this folder by its
// index.ts and the page by its export name (05 §5 "Page folders"), so a route that renders the page here proves the
// contract; before this folder existed the route rendered PageUnavailable.
import { fireEvent, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { createAppRoutes, defaultSources } from '../app/router';
import { createTestPlatform } from '../test/platform';
import { createTestServices, renderRoute } from '../test/render';
import { PROJECT_SITE_URL } from './DownloadPage';
import * as folder from './index';

const HEADING = 'Desktop app';

describe('DownloadPage', () => {
  it('is what /download renders in the app: the placeholder, not PageUnavailable', async () => {
    // createAppRoutes() with the build's real folders, as boot uses it. Public: nothing asks for /api/v1/me.
    renderRoute({ path: '/download' });
    expect(await screen.findByRole('heading', { level: 1, name: HEADING })).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: 'Not in this build yet' })).not.toBeInTheDocument();

    // 05 §15.4's text.
    expect(
      screen.getByText(
        'The isshoni desktop app is coming (Windows first). It keeps Discord and other voice apps out of what friends hear.',
      ),
    ).toBeInTheDocument();
    expect(
      screen.getByText('Until then, share from Chrome or Edge: pick a Window and keep Share audio on.'),
    ).toBeInTheDocument();

    // The project site, in a new tab without opener or referrer (05 §20).
    const site = screen.getByRole('link', { name: 'Project site' });
    expect(site).toHaveAttribute('href', 'https://moonwx.github.io/isshoni/');
    expect(site).toHaveAttribute('target', '_blank');
    expect(site).toHaveAttribute('rel', 'noopener noreferrer');
    expect(PROJECT_SITE_URL).toBe('https://moonwx.github.io/isshoni/');

    expect(screen.getByRole('link', { name: 'Back to isshoni' })).toHaveAttribute('href', '/');
    expect(screen.getByRole('main')).toContainElement(site);
  });

  it('is the download folder of the build, exported by name from its index', async () => {
    expect(Object.keys(folder)).toEqual(['DownloadPage']);
    const load = defaultSources().lazy.download;
    expect(load).toBeTypeOf('function');
    expect(await load?.()).toHaveProperty('DownloadPage', folder.DownloadPage);
    // Without the folder the same route is the router's stand-in: the page above comes from this folder.
    renderRoute({ path: '/download', routes: createAppRoutes({ lazy: {}, eager: {} }) });
    expect(await screen.findByRole('heading', { name: 'Not in this build yet' })).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: HEADING })).not.toBeInTheDocument();
  });

  it('leaves the link to the browser in a tab, and to the platform anywhere else', async () => {
    const openExternal = vi.fn<(url: string) => void>();
    const browser = renderRoute({
      path: '/download',
      services: createTestServices({ platform: createTestPlatform({ openExternal }) }),
    });
    const inTab = await screen.findByRole('link', { name: 'Project site' });
    // jsdom can't follow a link: stop it at the document, after the page's own handler had its say.
    let preventedByPage: boolean | undefined;
    const lastWord = (e: Event): void => {
      preventedByPage = e.defaultPrevented;
      e.preventDefault();
    };
    document.addEventListener('click', lastWord);
    fireEvent(inTab, new MouseEvent('click', { bubbles: true, cancelable: true }));
    document.removeEventListener('click', lastWord);
    expect(preventedByPage).toBe(false);
    expect(openExternal).not.toHaveBeenCalled();
    browser.unmount();

    // The desktop app (M2) has no tabs: the system browser opens the site.
    renderRoute({
      path: '/download',
      services: createTestServices({ platform: createTestPlatform({ kind: 'desktop', openExternal }) }),
    });
    const inApp = await screen.findByRole('link', { name: 'Project site' });
    const second = new MouseEvent('click', { bubbles: true, cancelable: true });
    fireEvent(inApp, second);
    expect(second.defaultPrevented).toBe(true);
    expect(openExternal.mock.calls).toEqual([['https://moonwx.github.io/isshoni/']]);
  });
});
