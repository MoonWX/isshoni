// The one-time tokens of /setup, /invite and /reset (05 §19.1 "auth/fragmentToken.ts", §20): read for each page,
// the fragment removed with replaceState before the first fetch (GET /api/v1/info included), stored for the tab and
// cleared once the form has succeeded. The second half boots the real app (startApp, the browser platform, the
// browser router and the real page folders) on a URL with a fragment and watches every fetch.
import { act, renderHook, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import type { ReactNode } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { AppProviders } from '../app/App';
import { startApp, type StartedApp } from '../app/boot';
import { createMemoryStorage } from '../platform/browser/storage';
import { installFakeRTC } from '../test/FakeRTCPeerConnection';
import { apiError, apiPath, infoFixture, meFixture, server } from '../test/msw';
import { createTestPlatform } from '../test/platform';
import { createTestServices } from '../test/render';
import { clearFragmentToken, readFragmentToken, useFragmentToken, type FragmentTokenKind } from './fragmentToken';

const KINDS: readonly FragmentTokenKind[] = ['setup', 'invite', 'reset'];

describe('readFragmentToken and clearFragmentToken', () => {
  it.each(KINDS)('reads the %s token from its own key and clears only that one', (kind) => {
    const session = createMemoryStorage({
      'isshoni.setup': 'setup-token',
      'isshoni.invite': 'invite-token',
      'isshoni.reset': 'reset-token',
    });
    expect(readFragmentToken(session, kind)).toBe(`${kind}-token`);
    clearFragmentToken(session, kind);
    expect(readFragmentToken(session, kind)).toBeNull();
    for (const other of KINDS.filter((k) => k !== kind)) {
      expect(readFragmentToken(session, other)).toBe(`${other}-token`);
    }
  });

  it('is null when the page was opened without a fragment', () => {
    expect(readFragmentToken(createMemoryStorage(), 'invite')).toBeNull();
    expect(readFragmentToken(createMemoryStorage({ 'isshoni.invite': '' }), 'invite')).toBeNull();
  });
});

describe('useFragmentToken', () => {
  function setup(stored: Record<string, string>) {
    const platform = createTestPlatform({
      storage: { local: createMemoryStorage(), session: createMemoryStorage(stored) },
    });
    const services = createTestServices({ platform });
    const wrapper = ({ children }: { children: ReactNode }) => (
      <AppProviders services={services}>{children}</AppProviders>
    );
    return { platform, ...renderHook(() => useFragmentToken('invite'), { wrapper }) };
  }

  it('keeps the token in memory after the stored copy is cleared', () => {
    const { platform, result, rerender } = setup({ 'isshoni.invite': 'tok' });
    expect(result.current.token).toBe('tok');
    act(() => {
      result.current.clear();
    });
    expect(platform.storage.session.get('isshoni.invite')).toBeNull();
    rerender();
    expect(result.current.token).toBe('tok');
  });

  it('is null without a stored token', () => {
    expect(setup({}).result.current.token).toBeNull();
  });

  it('stops listening for fragments when the page closes', () => {
    const added = vi.spyOn(window, 'addEventListener');
    const removed = vi.spyOn(window, 'removeEventListener');
    try {
      const { unmount } = setup({ 'isshoni.invite': 'tok' });
      const listener = added.mock.calls.find(([type]) => type === 'hashchange')?.[1];
      expect(listener).toBeTypeOf('function');
      unmount();
      expect(removed).toHaveBeenCalledWith('hashchange', listener);
    } finally {
      added.mockRestore();
      removed.mockRestore();
    }
  });
});

describe('a page opened with a token in the fragment', () => {
  const TOKEN = 'EXAMPLEfragmentTOKEN0123456789ab';

  interface Seen {
    /** Path and query of the request. */
    url: string;
    body: string | null;
    /** The address bar and the tab's storage at the moment of the request. */
    hash: string;
    href: string;
    stored: string | null;
  }

  let started: StartedApp | undefined;
  let uninstallRTC: (() => void) | undefined;
  let seen: Seen[];

  /** Boots the app at path#TOKEN and records what every fetch saw. */
  function boot(path: string, kind: FragmentTokenKind): void {
    window.history.replaceState(null, '', `${path}#${TOKEN}`);
    expect(window.location.hash).toBe(`#${TOKEN}`);
    const realFetch = globalThis.fetch;
    vi.spyOn(globalThis, 'fetch').mockImplementation((input, init) => {
      const url = new URL(input instanceof Request ? input.url : input);
      seen.push({
        url: url.pathname + url.search,
        body: typeof init?.body === 'string' ? init.body : null,
        hash: window.location.hash,
        href: window.location.href,
        stored: window.sessionStorage.getItem(`isshoni.${kind}`),
      });
      return realFetch(input, init);
    });
    const container = document.createElement('div');
    document.body.append(container);
    act(() => {
      started = startApp(container, { retryDelaysMs: [] });
    });
  }

  beforeEach(() => {
    seen = [];
    uninstallRTC = installFakeRTC();
    window.sessionStorage.clear();
  });

  afterEach(() => {
    act(() => {
      started?.stop();
    });
    started = undefined;
    uninstallRTC?.();
    vi.restoreAllMocks();
    window.sessionStorage.clear();
    window.history.replaceState(null, '', '/');
    document.body.innerHTML = '';
  });

  /** The session starts with the form's request; GET /api/v1/me follows it. */
  function mockApi(signIn: `/api/${string}`, status: number): void {
    let signedIn = false;
    server.use(
      http.get(apiPath('/api/v1/me'), () =>
        signedIn ? HttpResponse.json(meFixture()) : apiError(401, { code: 'unauthenticated' }),
      ),
      http.post(apiPath(signIn), () => {
        signedIn = true;
        return HttpResponse.json(
          { status: 'active', user: { id: 'k3m9p2qxw7ht', username: 'alex', role: 'user' } },
          { status },
        );
      }),
    );
  }

  /** One token page: its check, the request that signs in, and the form to fill in. */
  interface TokenPage {
    kind: FragmentTokenKind;
    path: string;
    check: () => Response;
    signIn: `/api/${string}`;
    status: number;
    /** The token's name in the sign-in request body. */
    tokenField: string;
    submit: string;
    /** Label → what to type. */
    fields: Record<string, string>;
  }

  it.each<TokenPage>([
    {
      kind: 'invite',
      path: '/invite',
      check: () =>
        HttpResponse.json({
          serverName: 'Test server',
          invitedBy: 'Alex',
          expiresAt: '2026-10-15T12:00:00.000Z',
          usesLeft: 9,
        }),
      signIn: '/api/v1/auth/register',
      status: 201,
      tokenField: 'inviteToken',
      submit: 'Create account',
      fields: { Username: 'bo', Password: 'correct horse battery' },
    },
    {
      kind: 'setup',
      path: '/setup',
      check: () => new HttpResponse(null, { status: 204 }),
      signIn: '/api/v1/auth/setup/complete',
      status: 201,
      tokenField: 'token',
      submit: 'Create account',
      fields: { Username: 'admin', Password: 'correct horse battery' },
    },
    {
      kind: 'reset',
      path: '/reset',
      check: () => HttpResponse.json({ username: 'Sam' }),
      signIn: '/api/v1/auth/reset/complete',
      status: 200,
      tokenField: 'token',
      submit: 'Save and log in',
      fields: { 'New password': 'a brand new passphrase' },
    },
  ])(
    '$path: the fragment is gone before the first request, the token travels only in JSON bodies, and it is cleared after the form succeeds',
    async ({ kind, path, check, signIn, status, tokenField, submit, fields }) => {
      server.use(
        http.get(apiPath('/api/v1/info'), () => HttpResponse.json(infoFixture({ setupRequired: kind === 'setup' }))),
        http.post(apiPath(`/api/v1/auth/${kind}/check`), check),
      );
      mockApi(signIn, status);
      boot(path, kind);

      // The page's form shows: the token reached the page although the address bar lost it.
      const user = userEvent.setup();
      for (const [label, value] of Object.entries(fields)) {
        await user.type(await screen.findByLabelText(label), value);
      }

      // The first request of all is GET /api/v1/info, and by then the fragment was gone and the token stashed.
      expect(seen[0]).toMatchObject({ url: '/api/v1/info', hash: '', stored: TOKEN });
      expect(window.location.pathname + window.location.search + window.location.hash).toBe(path);
      for (const request of seen) {
        expect(request.hash).toBe('');
        expect(request.href).not.toContain(TOKEN);
        // Never in a URL (05 §20).
        expect(request.url).not.toContain(TOKEN);
      }
      const checked = seen.find((r) => r.url === `/api/v1/auth/${kind}/check`);
      expect(JSON.parse(checked?.body ?? 'null')).toEqual({ token: TOKEN });
      // Requests before the check carry no token at all.
      expect(seen.slice(0, seen.indexOf(checked as Seen)).every((r) => !(r.body ?? '').includes(TOKEN))).toBe(true);

      await user.click(screen.getByRole('button', { name: submit }));
      await waitFor(() => {
        expect(window.location.pathname).toBe('/');
      });
      const completed = seen.find((r) => r.url === signIn);
      expect(JSON.parse(completed?.body ?? 'null')).toMatchObject({ [tokenField]: TOKEN });
      // Used up: gone from the tab's storage (05 §20), and never written to localStorage.
      expect(window.sessionStorage.getItem(`isshoni.${kind}`)).toBeNull();
      expect(JSON.stringify(Object.entries(window.localStorage))).not.toContain(TOKEN);
      for (const request of seen) expect(request.url).not.toContain(TOKEN);
    },
  );

  it('a reload before the form is sent still finds the token (the fragment is gone, the tab kept it)', async () => {
    server.use(
      http.get(apiPath('/api/v1/info'), () => HttpResponse.json(infoFixture())),
      http.get(apiPath('/api/v1/me'), () => apiError(401, { code: 'unauthenticated' })),
      http.post(apiPath('/api/v1/auth/reset/check'), () => HttpResponse.json({ username: 'Sam' })),
    );
    boot('/reset', 'reset');
    expect(await screen.findByLabelText('New password')).toBeInTheDocument();
    // "Reload": the app starts again on the URL the address bar now shows.
    act(() => {
      started?.stop();
    });
    document.body.innerHTML = '';
    expect(window.location.hash).toBe('');
    const container = document.createElement('div');
    document.body.append(container);
    act(() => {
      started = startApp(container, { retryDelaysMs: [] });
    });
    expect(await screen.findByLabelText('New password')).toBeInTheDocument();
    expect(await screen.findByLabelText('Username')).toHaveValue('Sam');
  });

  it('a new link pasted into the same tab (only the fragment changes, no reload): stashed, dropped, and used', async () => {
    const FRESH = 'FRESHinviteTOKEN0123456789abcdef';
    server.use(
      http.get(apiPath('/api/v1/info'), () => HttpResponse.json(infoFixture())),
      http.get(apiPath('/api/v1/me'), () => apiError(401, { code: 'unauthenticated' })),
      http.post(apiPath('/api/v1/auth/invite/check'), async ({ request }) => {
        const body = (await request.json()) as { token?: string };
        return body.token === FRESH
          ? HttpResponse.json({ serverName: 'Test server', expiresAt: '2026-10-15T12:00:00.000Z', usesLeft: 1 })
          : apiError(410, { code: 'invite_expired' });
      }),
    );
    boot('/invite', 'invite');
    expect(await screen.findByText('This invite link has expired. Ask for a new one.')).toBeInTheDocument();
    expect(window.location.href).not.toContain('#');

    // The friend pastes the new link over /invite: a fragment navigation, so the page isn't loaded again.
    window.location.hash = `#${FRESH}`;

    expect(await screen.findByRole('heading', { name: "You're invited to Test server" })).toBeInTheDocument();
    expect(await screen.findByLabelText('Username')).toBeInTheDocument();
    expect(window.location.hash).toBe('');
    expect(window.location.href).not.toContain(FRESH);
    expect(window.sessionStorage.getItem('isshoni.invite')).toBe(FRESH);
    const checks = seen.filter((r) => r.url === '/api/v1/auth/invite/check');
    expect(checks.map((r) => JSON.parse(r.body ?? 'null') as unknown)).toEqual([{ token: TOKEN }, { token: FRESH }]);
    // The second check went out after the fragment was gone again.
    expect(checks[1]).toMatchObject({ hash: '', stored: FRESH });
    for (const request of seen) expect(request.url).not.toContain(FRESH);
  });

  it('a fragment on another page is none of a token page’s business', async () => {
    server.use(
      http.get(apiPath('/api/v1/info'), () => HttpResponse.json(infoFixture())),
      http.get(apiPath('/api/v1/me'), () => apiError(401, { code: 'unauthenticated' })),
    );
    window.history.replaceState(null, '', '/login');
    const container = document.createElement('div');
    document.body.append(container);
    act(() => {
      started = startApp(container, { retryDelaysMs: [] });
    });
    await screen.findByRole('heading', { name: 'Log in to Test server' });
    window.location.hash = '#section';
    await new Promise((r) => setTimeout(r, 20));
    expect(window.location.hash).toBe('#section');
    expect(Object.keys(window.sessionStorage)).toEqual([]);
  });
});
