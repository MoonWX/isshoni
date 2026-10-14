// /admin/welcome, the setup wizard's steps 2 and 3 (05 §14.1), in the app's routes against MSW. The connection test
// itself is a double (conntest/ has its own tests): what matters here is that step 2 runs it, and what "Continue"
// looks like next to its result. Step 3 creates the invite (#22), shows it, and "Done" sets setupWizardDone (#41).
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { StrictMode } from 'react';
import { createMemoryRouter, RouterProvider } from 'react-router';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { AppProviders } from '../app/App';
import { createAppRoutes } from '../app/router';
import { mockSession, onPost, pageSources, renderPage, ROOM_HEADING } from '../auth/testing/harness';
import { runConnTest, type ConnTestResult } from '../conntest/runConnTest';
import { resultFixture } from '../conntest/testing';
import type { CreateInviteResponse, SettingsResponse } from '../protocol/api.gen';
import { queryKeys } from '../protocol/queryKeys';
import { apiError, apiPath, meFixture, server } from '../test/msw';
import { createTestServices } from '../test/render';
import { welcomePath, welcomeStep } from './WelcomePage';

vi.mock(import('../conntest/runConnTest'), async (importOriginal) => ({
  ...(await importOriginal()),
  runConnTest: vi.fn(),
}));

const STEP_2 = 'Test the connection';
const STEP_3 = 'Invite your friends';
const LINK = 'http://localhost:3000/invite#EXAMPLEinviteTOKEN0123456789abcd';

function deferred<T>() {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((res) => {
    resolve = res;
  });
  return { promise, resolve };
}

/** The connection test answers with these results, one per run (the last one again after that). */
function testFinds(...results: (ConnTestResult | Promise<ConnTestResult>)[]): void {
  let run = 0;
  vi.mocked(runConnTest).mockImplementation(() => {
    const result = results[Math.min(run++, results.length - 1)];
    return result === undefined ? Promise.reject(new Error('no result')) : Promise.resolve(result);
  });
}

function inviteResponse(): CreateInviteResponse {
  return {
    url: LINK,
    invite: {
      id: 'h6j8k0m2n4p6',
      note: '',
      createdBy: { id: 'a1b2c3d4e5f6', username: 'admin' },
      createdAt: '2026-10-01T12:00:00.000Z',
      expiresAt: '2026-10-08T12:00:00.000Z',
      maxUses: 10,
      uses: 0,
      state: 'active',
      redeemedBy: [],
    },
  };
}

/** POST /api/v1/invites that creates the invite; returns the bodies it was sent. */
function invitesWork(): unknown[] {
  return onPost('/api/v1/invites', () => HttpResponse.json(inviteResponse(), { status: 201 }));
}

/** PATCH /api/v1/admin/settings; returns the bodies it was sent. */
function settingsPatch(respond: () => Response = settingsSaved): unknown[] {
  const bodies: unknown[] = [];
  server.use(
    http.patch(apiPath('/api/v1/admin/settings'), async ({ request }) => {
      bodies.push(await request.json());
      return respond();
    }),
  );
  return bodies;
}

function settingsSaved(): Response {
  const settings = {
    serverName: '',
    registrationMode: 'invite',
    inviteDefaultTtlHours: 168,
    inviteDefaultMaxUses: 10,
    membersCanInvite: false,
    maxParticipantsPerRoom: 0,
    maxSharesPerRoom: 0,
    maxShareBitrateKbps: 0,
    transferAlertGb: 0,
    updateCheck: true,
    minClientVersion: '',
    setupWizardDone: true,
  } satisfies SettingsResponse['settings'];
  return HttpResponse.json({
    settings,
    defaults: { ...settings, setupWizardDone: false },
    locked: [],
  } satisfies SettingsResponse);
}

function renderWelcome(path: string = welcomePath(2)) {
  mockSession(meFixture({ admin: true }));
  return renderPage({ path });
}

/** The stepper's steps, as screen readers hear them. */
function steps(): string[] {
  return within(screen.getByRole('list', { name: 'Setup' }))
    .getAllByRole('listitem')
    .map((li) => li.textContent);
}

function currentStep(): HTMLElement | undefined {
  return within(screen.getByRole('list', { name: 'Setup' }))
    .getAllByRole('listitem')
    .find((li) => li.getAttribute('aria-current') === 'step');
}

beforeEach(() => {
  vi.mocked(runConnTest).mockReset();
  testFinds(resultFixture());
});

describe('welcomeStep and welcomePath', () => {
  it.each([
    ['3', 3],
    ['2', 2],
    [null, 2],
    ['1', 2],
    ['4', 2],
    ['', 2],
  ] as const)('?step=%s is step %d', (raw, step) => {
    expect(welcomeStep(raw)).toBe(step);
  });

  it('names the step in the query', () => {
    expect(welcomePath(2)).toBe('/admin/welcome?step=2');
    expect(welcomePath(3)).toBe('/admin/welcome?step=3');
  });
});

describe('step 2: the connection test', () => {
  it('runs the test at once, once, with step 1 done and step 2 current', async () => {
    const { services } = renderWelcome();
    expect(await screen.findByRole('heading', { name: STEP_2, level: 1 })).toBeInTheDocument();
    expect(steps()).toEqual([
      'Step 1 of 3, done: Create the admin',
      '2Step 2 of 3: Connection test',
      '3Step 3 of 3: Invite friends',
    ]);
    expect(currentStep()).toHaveTextContent('Connection test');

    expect(await screen.findByText('Friends get the best quality.')).toBeInTheDocument();
    expect(runConnTest).toHaveBeenCalledTimes(1);
    expect(vi.mocked(runConnTest).mock.calls[0]?.[0]).toBe(services.platform);
    expect(screen.getByText('Media over UDP (port 7882)')).toBeInTheDocument();
  });

  it.each(['/admin/welcome', '/admin/welcome?step=1', '/admin/welcome?step=next'])('%s is step 2', async (path) => {
    renderWelcome(path);
    expect(await screen.findByRole('heading', { name: STEP_2, level: 1 })).toBeInTheDocument();
    expect(currentStep()).toHaveTextContent('Connection test');
    await waitFor(() => {
      expect(runConnTest).toHaveBeenCalledTimes(1);
    });
  });

  it('offers Continue before the result, as the second button', async () => {
    const run = deferred<ConnTestResult>();
    testFinds(run.promise);
    renderWelcome();
    const next = await screen.findByRole('button', { name: 'Continue' });
    expect(next.className).toMatch(/secondary/);
    expect(await screen.findByText(/Testing your connection/)).toBeInTheDocument();
    run.resolve(resultFixture());
    expect(await screen.findByText('Friends get the best quality.')).toBeInTheDocument();
  });

  it('makes Continue the main action after a green result; it goes to step 3, and focus to its heading', async () => {
    invitesWork();
    const { router } = renderWelcome();
    expect(await screen.findByText('Friends get the best quality.')).toBeInTheDocument();
    const next = screen.getByRole('button', { name: 'Continue' });
    expect(next.className).toMatch(/primary/);
    expect(screen.getByRole('button', { name: 'Test again' }).className).toMatch(/secondary/);

    await userEvent.click(next);
    const heading = await screen.findByRole('heading', { name: STEP_3, level: 1 });
    expect(router.state.location.pathname + router.state.location.search).toBe('/admin/welcome?step=3');
    await waitFor(() => {
      expect(heading).toHaveFocus();
    });
    // A new history entry: Back returns to the test.
    expect(router.state.historyAction).toBe('PUSH');
  });

  it.each([
    ['amber (UDP blocked)', resultFixture({ udp: 'timeout' }), 'Works, but video may stutter on weak networks.'],
    [
      'red (nothing gets through)',
      resultFixture({ udp: 'timeout', tcp443: 'timeout', tcp7882: 'timeout' }),
      "Friends can't receive video yet.",
    ],
    ['not finished (rate limited)', resultFixture({ udp: 'rate_limited' }), /Couldn't finish the test/],
  ])('after a result that is %s, Continue is secondary to "Test again" and still goes on', async (_n, result, text) => {
    testFinds(result);
    invitesWork();
    const { router } = renderWelcome();
    expect(await screen.findByText(text)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Test again' }).className).toMatch(/primary/);
    const next = screen.getByRole('button', { name: 'Continue' });
    expect(next.className).toMatch(/secondary/);
    await userEvent.click(next);
    expect(await screen.findByRole('heading', { name: STEP_3, level: 1 })).toBeInTheDocument();
    expect(router.state.location.search).toBe('?step=3');
  });

  it('steps Continue back while "Test again" runs after a green result, and forward when it is green again', async () => {
    const second = deferred<ConnTestResult>();
    testFinds(resultFixture(), second.promise);
    renderWelcome();
    expect(await screen.findByText('Friends get the best quality.')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Continue' }).className).toMatch(/primary/);

    await userEvent.click(screen.getByRole('button', { name: 'Test again' }));
    expect(await screen.findByText(/Testing your connection/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Continue' }).className).toMatch(/secondary/);

    second.resolve(resultFixture());
    expect(await screen.findByText('Friends get the best quality.')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Continue' }).className).toMatch(/primary/);
    expect(runConnTest).toHaveBeenCalledTimes(2);
  });

  it('shows fix text to the admin (the wizard is for admins only)', async () => {
    testFinds(resultFixture({ udp: 'timeout' }));
    renderWelcome();
    expect(await screen.findByRole('heading', { name: 'How to fix it' })).toBeInTheDocument();
    expect(screen.getByText('sudo ufw allow 7882/udp')).toBeInTheDocument();
    expect(screen.queryByText('Send this to your admin:')).not.toBeInTheDocument();
  });
});

describe('step 3: invite friends', () => {
  it('creates one invite with the server defaults and shows its link once, with all three steps', async () => {
    const created = invitesWork();
    const { services } = renderWelcome(welcomePath(3));
    expect(await screen.findByRole('heading', { name: STEP_3, level: 1 })).toBeInTheDocument();
    expect(steps()).toEqual([
      'Step 1 of 3, done: Create the admin',
      'Step 2 of 3, done: Connection test',
      '3Step 3 of 3: Invite friends',
    ]);

    expect(await screen.findByRole('textbox', { name: 'Invite link' })).toHaveValue(LINK);
    expect(screen.getByRole('img', { name: 'QR code of the invite link' })).toBeInTheDocument();
    expect(screen.getByText(/^Works for up to 10 people, until /)).toBeInTheDocument();
    expect(screen.getByText(/can't be shown again/)).toBeInTheDocument();
    // Every field left to the server: 7 days and 10 uses unless the admin changed the defaults.
    expect(created).toEqual([{}]);
    // Step 3 has no connection test.
    expect(runConnTest).not.toHaveBeenCalled();
    // The invite is in no REST cache: only the page's state has the link.
    expect(
      services.queryClient
        .getQueryCache()
        .getAll()
        .some((q) => JSON.stringify(q.state.data).includes(LINK)),
    ).toBe(false);
  });

  it('creates one invite under StrictMode too', async () => {
    const created = invitesWork();
    mockSession(meFixture({ admin: true }));
    const router = createMemoryRouter(createAppRoutes(pageSources()), { initialEntries: [welcomePath(3)] });
    render(
      <StrictMode>
        <AppProviders services={createTestServices()}>
          <RouterProvider router={router} />
        </AppProviders>
      </StrictMode>,
    );
    expect(await screen.findByRole('textbox', { name: 'Invite link' })).toHaveValue(LINK);
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(created).toHaveLength(1);
  });

  it('marks the invites list stale, so Admin → Invites shows the new one', async () => {
    invitesWork();
    const { services } = renderWelcome(welcomePath(3));
    services.queryClient.setQueryData([...queryKeys.invites, { state: 'all' }], { invites: [] });
    await screen.findByRole('textbox', { name: 'Invite link' });
    await waitFor(() => {
      expect(services.queryClient.getQueryState([...queryKeys.invites, { state: 'all' }])?.isInvalidated).toBe(true);
    });
  });

  it('"Done" sets setupWizardDone and goes to the Lounge, replacing the step', async () => {
    invitesWork();
    const patched = settingsPatch();
    const { router, services } = renderWelcome(welcomePath(3));
    await screen.findByRole('textbox', { name: 'Invite link' });
    services.queryClient.setQueryData(queryKeys.adminSettings, { stale: true });

    await userEvent.click(screen.getByRole('button', { name: 'Done, go to the Lounge' }));
    expect(await screen.findByRole('heading', { name: ROOM_HEADING })).toBeInTheDocument();
    expect(patched).toEqual([{ setupWizardDone: true }]);
    expect(router.state.location.pathname).toBe('/r/lounge');
    expect(router.state.historyAction).toBe('REPLACE');
    // The settings (and /info with them) are read again where they are shown.
    expect(services.queryClient.getQueryState(queryKeys.adminSettings)?.isInvalidated).toBe(true);
  });

  it.each([
    [
      'registration_closed',
      () => apiError(403, { code: 'registration_closed' }),
      "Couldn't make an invite link. This server isn't taking new accounts.",
    ],
    [
      'limit_reached',
      () => apiError(409, { code: 'limit_reached', params: { limit: 'invites' } }),
      "Couldn't make an invite link. This server has as many active invites as it allows. Revoke some first.",
    ],
    ['offline', () => HttpResponse.error(), /^Couldn't make an invite link\. You're offline/],
  ])('%s: says why there is no link, offers "Try again", and still lets the admin finish', async (_n, fail, shown) => {
    let failing = true;
    const created = onPost('/api/v1/invites', () =>
      failing ? fail() : HttpResponse.json(inviteResponse(), { status: 201 }),
    );
    const patched = settingsPatch();
    renderWelcome(welcomePath(3));
    expect(await screen.findByRole('alert')).toHaveTextContent(shown);
    expect(screen.queryByRole('textbox', { name: 'Invite link' })).not.toBeInTheDocument();

    failing = false;
    await userEvent.click(screen.getByRole('button', { name: 'Try again' }));
    expect(await screen.findByRole('textbox', { name: 'Invite link' })).toHaveValue(LINK);
    expect(created).toHaveLength(2);
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole('button', { name: 'Done, go to the Lounge' }));
    expect(await screen.findByRole('heading', { name: ROOM_HEADING })).toBeInTheDocument();
    expect(patched).toEqual([{ setupWizardDone: true }]);
  });

  it('finishes without a link too', async () => {
    onPost('/api/v1/invites', () => apiError(403, { code: 'registration_closed' }));
    const patched = settingsPatch();
    renderWelcome(welcomePath(3));
    await screen.findByRole('alert');
    await userEvent.click(screen.getByRole('button', { name: 'Done, go to the Lounge' }));
    expect(await screen.findByRole('heading', { name: ROOM_HEADING })).toBeInTheDocument();
    expect(patched).toEqual([{ setupWizardDone: true }]);
  });

  it('stays on the step, with the link, when "Done" fails', async () => {
    invitesWork();
    const patched = settingsPatch(() => apiError(500, { code: 'internal', requestId: 'req-9' }));
    const { router } = renderWelcome(welcomePath(3));
    await screen.findByRole('textbox', { name: 'Invite link' });
    await userEvent.click(screen.getByRole('button', { name: 'Done, go to the Lounge' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(
      "Couldn't finish the setup. Something went wrong on the server. Reference: req-9",
    );
    expect(patched).toHaveLength(1);
    expect(router.state.location.search).toBe('?step=3');
    expect(screen.getByRole('textbox', { name: 'Invite link' })).toHaveValue(LINK);
    expect(screen.getByRole('button', { name: 'Done, go to the Lounge' })).toBeEnabled();
  });
});

describe('who sees the wizard', () => {
  it.each([welcomePath(2), welcomePath(3)])('a member gets Not found at %s, and nothing runs', async (path) => {
    const created = invitesWork();
    mockSession(meFixture());
    renderPage({ path });
    expect(await screen.findByRole('heading', { name: 'Nothing here' })).toBeInTheDocument();
    expect(runConnTest).not.toHaveBeenCalled();
    expect(created).toEqual([]);
  });

  it('a signed-out visitor goes to the login page, and back here after it', async () => {
    mockSession(null);
    const { router } = renderPage({ path: welcomePath(3) });
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/login');
    });
    expect(router.state.location.search).toBe(`?next=${encodeURIComponent(welcomePath(3))}`);
  });
});
