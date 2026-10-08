// The logout flow (05 §15.1): its order, what a failure leaves in place, and the redirect in this tab and in the
// user's other tabs (05 §6.2: BroadcastChannel('isshoni') {type: 'logout'}).
import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { createMemoryRouter, type DataRouter } from 'react-router';

import { startApp, type StartedApp } from '../app/boot';
import { createAppRoutes } from '../app/router';
import { CHANNEL_NAME } from '../app/session';
import { queryKeys } from '../protocol/queryKeys';
import { NetworkError } from '../protocol/rest';
import { installFakeRTC } from '../test/FakeRTCPeerConnection';
import { apiError, apiPath, infoFixture, meFixture, roomsFixture, server } from '../test/msw';
import { createTestPlatform } from '../test/platform';
import { createTestServices, renderRoute } from '../test/render';
import { addLogoutStep, logout, PUSH_UNSUBSCRIBE_TIMEOUT_MS, type LogoutPhase, type LogoutStep } from './logout';
import { mockSession, networkError, noContent, onPost, pageSources } from './testing/harness';
import { useLogout } from './useLogout';
import { endSession, startSession, SessionNotKeptError } from './session';

/** A signed-in tab's cache: the user, a resource of theirs, and the public /info. */
function signedInServices() {
  const services = createTestServices();
  services.queryClient.setQueryData(queryKeys.me, meFixture());
  services.queryClient.setQueryData(queryKeys.rooms, roomsFixture());
  services.queryClient.setQueryData(queryKeys.meSessions, { sessions: [] });
  return services;
}

/** Collects the logout messages other tabs would get. */
function listenAsAnotherTab(): { messages: unknown[]; close: () => void } {
  const channel = new BroadcastChannel(CHANNEL_NAME);
  const messages: unknown[] = [];
  channel.onmessage = (e: MessageEvent<unknown>) => messages.push(e.data);
  return {
    messages,
    close: () => {
      channel.close();
    },
  };
}

describe('logout()', () => {
  let removers: (() => void)[] = [];
  let other: ReturnType<typeof listenAsAnotherTab>;

  beforeEach(() => {
    other = listenAsAnotherTab();
  });

  afterEach(() => {
    for (const remove of removers) remove();
    removers = [];
    other.close();
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  function step(phase: LogoutPhase, fn: LogoutStep): void {
    removers.push(addLogoutStep(phase, fn));
  }

  it('runs in the order of 05 §15.1: local steps, the request, the push subscription, the other tabs, the cache', async () => {
    const order: string[] = [];
    const services = signedInServices();
    // The connection registers first (it exists before any share does); shares still stop first.
    step('signal', async () => {
      await Promise.resolve();
      order.push('SignalClient.stop()');
      return undefined;
    });
    step('shares', () => {
      order.push('stop local shares');
      return undefined;
    });
    onPost('/api/v1/auth/logout', (body) => {
      order.push(`POST logout ${JSON.stringify(body)}`);
      // Still signed in on this side while the request runs.
      expect(services.queryClient.getQueryData(queryKeys.me)).not.toBeNull();
      return noContent();
    });
    const unsubscribe = vi.fn(() => {
      order.push('PushSubscription.unsubscribe()');
      return Promise.resolve(true);
    });
    vi.stubGlobal('navigator', {
      serviceWorker: {
        getRegistration: () =>
          Promise.resolve({ pushManager: { getSubscription: () => Promise.resolve({ unsubscribe }) } }),
      },
    });

    await logout(services);
    order.push(`me is ${JSON.stringify(services.queryClient.getQueryData(queryKeys.me))}`);

    expect(order).toEqual([
      'stop local shares',
      'SignalClient.stop()',
      'POST logout {}',
      'PushSubscription.unsubscribe()',
      'me is null',
    ]);
    await waitFor(() => {
      expect(other.messages).toEqual([{ type: 'logout' }]);
    });
    // Nothing of the user stays cached; the public /info does.
    expect(services.queryClient.getQueryData(queryKeys.rooms)).toBeUndefined();
    expect(services.queryClient.getQueryData(queryKeys.meSessions)).toBeUndefined();
    expect(services.queryClient.getQueryData(queryKeys.info)).toEqual(infoFixture());
  });

  it('works in a browser without a service worker, and in one whose service worker has no push', async () => {
    onPost('/api/v1/auth/logout', noContent);
    vi.stubGlobal('navigator', {});
    await expect(logout(signedInServices())).resolves.toBeUndefined();
    vi.stubGlobal('navigator', { serviceWorker: { getRegistration: () => Promise.resolve({}) } });
    await expect(logout(signedInServices())).resolves.toBeUndefined();
    vi.stubGlobal('navigator', { serviceWorker: { getRegistration: () => Promise.reject(new Error('blocked')) } });
    await expect(logout(signedInServices())).resolves.toBeUndefined();
  });

  it('a push service that never answers holds the rest up only until the time limit', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    onPost('/api/v1/auth/logout', noContent);
    const services = signedInServices();
    const unsubscribe = vi.fn(() => new Promise<boolean>(() => undefined));
    vi.stubGlobal('navigator', {
      serviceWorker: {
        getRegistration: () =>
          Promise.resolve({ pushManager: { getSubscription: () => Promise.resolve({ unsubscribe }) } }),
      },
    });

    let settled = false;
    const running = logout(services).finally(() => {
      settled = true;
    });
    await vi.waitFor(() => {
      expect(unsubscribe).toHaveBeenCalledOnce();
    });
    // The order of 05 §15.1 holds while there is still time: step 4 comes before the other tabs and the cache.
    await vi.advanceTimersByTimeAsync(PUSH_UNSUBSCRIBE_TIMEOUT_MS / 2);
    expect(settled).toBe(false);
    expect(services.queryClient.getQueryData(queryKeys.me)).toEqual(meFixture());
    expect(other.messages).toEqual([]);

    await vi.advanceTimersByTimeAsync(PUSH_UNSUBSCRIBE_TIMEOUT_MS / 2);
    await expect(running).resolves.toBeUndefined();
    expect(services.queryClient.getQueryData(queryKeys.me)).toBeNull();
    expect(services.queryClient.getQueryData(queryKeys.rooms)).toBeUndefined();
    await vi.waitFor(() => {
      expect(other.messages).toEqual([{ type: 'logout' }]);
    });
  });

  it('a step that throws does not keep the user signed in', async () => {
    const posted = onPost('/api/v1/auth/logout', noContent);
    const services = signedInServices();
    const after = vi.fn(() => undefined);
    step('shares', () => {
      throw new Error('capture already gone');
    });
    step('signal', after);
    await logout(services);
    expect(after).toHaveBeenCalledOnce();
    expect(posted).toHaveLength(1);
    expect(services.queryClient.getQueryData(queryKeys.me)).toBeNull();
  });

  it('when the server cannot be reached: rejects, undoes the steps in reverse, and the user stays signed in', async () => {
    onPost('/api/v1/auth/logout', networkError);
    const services = signedInServices();
    const order: string[] = [];
    step('shares', () => () => order.push('undo first'));
    step('signal', () => Promise.resolve(() => order.push('undo second')));
    step('signal', () => undefined);
    await expect(logout(services)).rejects.toBeInstanceOf(NetworkError);
    expect(order).toEqual(['undo second', 'undo first']);
    expect(services.queryClient.getQueryData(queryKeys.me)).toEqual(meFixture());
    expect(services.queryClient.getQueryData(queryKeys.rooms)).toEqual(roomsFixture());
    await new Promise((r) => setTimeout(r, 20));
    expect(other.messages).toEqual([]);
  });

  it('a server error keeps the user signed in too', async () => {
    onPost('/api/v1/auth/logout', () => apiError(500, { code: 'internal' }));
    const services = signedInServices();
    await expect(logout(services)).rejects.toMatchObject({ code: 'internal' });
    expect(services.queryClient.getQueryData(queryKeys.me)).toEqual(meFixture());
  });

  it('an answer that says the session is already gone counts as logged out', async () => {
    onPost('/api/v1/auth/logout', () => apiError(401, { code: 'unauthenticated' }));
    const services = signedInServices();
    await logout(services);
    expect(services.queryClient.getQueryData(queryKeys.me)).toBeNull();
  });

  it('a removed step no longer runs', async () => {
    onPost('/api/v1/auth/logout', noContent);
    const fn = vi.fn(() => undefined);
    const remove = addLogoutStep('signal', fn);
    remove();
    await logout(signedInServices());
    expect(fn).not.toHaveBeenCalled();
  });
});

describe('startSession and endSession', () => {
  it('startSession loads the new user and drops what the previous one left', async () => {
    const session = mockSession(meFixture({ admin: true }));
    const services = createTestServices();
    services.queryClient.setQueryData(queryKeys.me, null);
    services.queryClient.setQueryData(queryKeys.rooms, roomsFixture());
    await expect(startSession(services.queryClient)).resolves.toEqual(meFixture({ admin: true }));
    expect(services.queryClient.getQueryData(queryKeys.me)).toEqual(meFixture({ admin: true }));
    expect(services.queryClient.getQueryData(queryKeys.rooms)).toBeUndefined();
    expect(services.queryClient.getQueryData(queryKeys.info)).toEqual(infoFixture());
    expect(session.meCalls).toBe(1);
  });

  it('startSession rejects when the server still says "signed out" (the cookie was not kept)', async () => {
    mockSession(null);
    await expect(startSession(createTestServices().queryClient)).rejects.toBeInstanceOf(SessionNotKeptError);
  });

  it('startSession resolves undefined when GET /api/v1/me cannot be reached, after one try', async () => {
    let calls = 0;
    server.use(
      http.get(apiPath('/api/v1/me'), () => {
        calls++;
        return HttpResponse.error();
      }),
    );
    await expect(startSession(createTestServices().queryClient)).resolves.toBeUndefined();
    expect(calls).toBe(1);
  });

  it('endSession tells mounted guards at once: ["me"] is set to null, not removed', () => {
    const services = signedInServices();
    const seen: unknown[] = [];
    const unsubscribe = services.queryClient.getQueryCache().subscribe((e) => {
      const key = e.query.queryKey as readonly unknown[];
      if (key[0] === 'me' && key.length === 1) seen.push(e.type);
    });
    endSession(services.queryClient);
    unsubscribe();
    expect(services.queryClient.getQueryData(queryKeys.me)).toBeNull();
    expect(seen).not.toContain('removed');
  });
});

describe('logout and the tabs', () => {
  const user = userEvent.setup();

  /** A guarded page with a "Sign out" button, as the account menu will have. */
  function AccountWithSignOut() {
    const { logout: signOut, pending } = useLogout();
    return (
      <main>
        <h1>account</h1>
        <button type="button" disabled={pending} onClick={() => void signOut()}>
          Sign out
        </button>
      </main>
    );
  }

  function routes() {
    const sources = pageSources();
    return createAppRoutes({
      ...sources,
      lazy: { ...sources.lazy, account: () => Promise.resolve({ AccountPage: AccountWithSignOut }) },
    });
  }

  it('this tab: a guarded page goes to /login?next=<path> once the session has ended', async () => {
    const session = mockSession(meFixture());
    const posted = onPost('/api/v1/auth/logout', () => {
      session.me = null;
      return noContent();
    });
    const { router } = renderRoute({ path: '/account?tab=pw', routes: routes() });
    await user.click(await screen.findByRole('button', { name: 'Sign out' }));
    expect(await screen.findByRole('heading', { name: 'Log in to Test server' })).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/login');
    expect(router.state.location.search).toBe(`?next=${encodeURIComponent('/account?tab=pw')}`);
    expect(posted).toEqual([{}]);
  });

  it('this tab: stays signed in, with a toast, when the server cannot be reached', async () => {
    mockSession(meFixture());
    onPost('/api/v1/auth/logout', () => apiError(503, { code: 'server_shutdown' }));
    const { router, services } = renderRoute({ path: '/account', routes: routes() });
    const button = await screen.findByRole('button', { name: 'Sign out' });
    await user.click(button);
    await waitFor(() => {
      expect(services.ui.getState().toasts.map((t) => [t.kind, t.message])).toEqual([
        ['error', "Couldn't sign out. The server is restarting."],
      ]);
    });
    expect(router.state.location.pathname).toBe('/account');
    expect(button).toBeEnabled();
  });

  describe('two tabs of one browser', () => {
    interface Tab {
      container: HTMLElement;
      router: DataRouter;
      app: StartedApp;
    }

    let tabs: Tab[] = [];
    let uninstallRTC: (() => void) | undefined;

    /** Boots the app (startApp, as main.tsx does) in its own container: one tab. */
    function openTab(path: string, stored: Record<string, string> = {}): Tab {
      const container = document.createElement('div');
      document.body.append(container);
      const platform = createTestPlatform();
      for (const [k, v] of Object.entries(stored)) platform.storage.session.set(k, v);
      let router: DataRouter | undefined;
      let app: StartedApp | undefined;
      act(() => {
        app = startApp(container, {
          platform,
          location: { pathname: path, search: '', hash: '' },
          history: { state: null, replaceState: vi.fn() },
          retryDelaysMs: [],
          routes,
          createRouter: (r) => (router = createMemoryRouter(r, { initialEntries: [path] })),
        });
      });
      const tab = {
        container,
        app: app as StartedApp,
        get router(): DataRouter {
          if (!router) throw new Error('the router is made once /info has answered');
          return router;
        },
      };
      tabs.push(tab);
      return tab;
    }

    beforeEach(() => {
      uninstallRTC = installFakeRTC();
    });

    afterEach(() => {
      act(() => {
        for (const tab of tabs) tab.app.stop();
      });
      tabs = [];
      uninstallRTC?.();
      document.body.innerHTML = '';
    });

    it('a logout in tab A redirects tab B to the login page', async () => {
      const session = mockSession(meFixture());
      const posted = onPost('/api/v1/auth/logout', () => {
        session.me = null;
        return noContent();
      });
      const a = openTab('/account');
      const b = openTab('/r/lounge');
      const tabA = within(a.container);
      const tabB = within(b.container);
      await tabA.findByRole('heading', { name: 'account' });
      await tabB.findByRole('heading', { name: 'the room' });
      const asked = session.meCalls;

      await user.click(tabA.getByRole('button', { name: 'Sign out' }));

      expect(await tabB.findByRole('heading', { name: 'Log in to Test server' })).toBeInTheDocument();
      expect(b.router.state.location.pathname).toBe('/login');
      expect(b.router.state.location.search).toBe(`?next=${encodeURIComponent('/r/lounge')}`);
      expect(b.app.services?.queryClient.getQueryData(queryKeys.me)).toBeNull();
      // Tab B never asked the server: the message from tab A cleared its ['me'], and its guard did the rest.
      expect(session.meCalls).toBe(asked);
      // Tab A went the same way, and the session ended once.
      expect(await tabA.findByRole('heading', { name: 'Log in to Test server' })).toBeInTheDocument();
      expect(a.router.state.location.pathname).toBe('/login');
      expect(posted).toEqual([{}]);
    });

    it('a logout in tab A leaves tab B alone while it is on a public page', async () => {
      const session = mockSession(meFixture());
      onPost('/api/v1/auth/logout', () => {
        session.me = null;
        return noContent();
      });
      onPost('/api/v1/auth/invite/check', () =>
        HttpResponse.json({ serverName: 'Test server', expiresAt: '2026-10-15T12:00:00.000Z', usesLeft: 1 }),
      );
      const a = openTab('/account');
      const b = openTab('/invite', { 'isshoni.invite': 'EXAMPLEinviteTOKEN0123456789abcd' });
      const tabA = within(a.container);
      const tabB = within(b.container);
      await tabB.findByText("You're signed in as alex.");

      await user.click(await tabA.findByRole('button', { name: 'Sign out' }));

      // The invite page stays where it is and now offers its form.
      expect(await tabB.findByRole('heading', { name: "You're invited to Test server" })).toBeInTheDocument();
      expect(await tabB.findByLabelText('Username')).toBeInTheDocument();
      expect(b.router.state.location.pathname).toBe('/invite');
    });
  });
});
