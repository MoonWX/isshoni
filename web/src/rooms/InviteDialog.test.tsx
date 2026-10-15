// "Invite friends" in the room (05 §11.2, §14.1): each opening of the dialog creates one invite with the server's
// defaults (POST /api/v1/invites {}) and shows its link once in setup/'s InviteLinkCard; closing it forgets the link.
import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { StrictMode, useState } from 'react';
import { createMemoryRouter, RouterProvider } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { AppProviders } from '../app/App';
import type { CreateInviteResponse } from '../protocol/api.gen';
import { queryKeys } from '../protocol/queryKeys';
import { apiError, apiPath, meFixture, server } from '../test/msw';
import { createTestServices, renderRoute } from '../test/render';
import { InviteDialog } from './InviteDialog';
import { stubNativeDialog } from './testing/page';

const TITLE = 'Invite friends';

/** The link of the nth invite the server made (1, 2, …). */
const linkOf = (n: number): string => `http://localhost:3000/invite#EXAMPLEinviteTOKEN${String(n).padStart(4, '0')}`;

function inviteResponse(n: number): CreateInviteResponse {
  return {
    url: linkOf(n),
    invite: {
      id: `h6j8k0m2n4p${String(n)}`,
      note: '',
      createdBy: { id: 'k3m9p2qxw7ht', username: 'alex' },
      createdAt: '2026-10-01T12:00:00.000Z',
      expiresAt: '2026-10-08T12:00:00.000Z',
      maxUses: 10,
      uses: 0,
      state: 'active',
      redeemedBy: [],
    },
  };
}

/**
 * POST /api/v1/invites: answers with each response in turn (the last one again after that); a response of null
 * makes the invite. Returns the bodies it was sent.
 */
function invites(...responses: (Response | null)[]): unknown[] {
  const bodies: unknown[] = [];
  server.use(
    http.post(apiPath('/api/v1/invites'), async ({ request }) => {
      bodies.push(await request.json());
      const response = responses[Math.min(bodies.length - 1, responses.length - 1)] ?? null;
      return response ?? HttpResponse.json(inviteResponse(bodies.length), { status: 201 });
    }),
  );
  return bodies;
}

/** The dialog behind a button that opens it, as the account menu has it: kept mounted, closed through `open`. */
function Opener({ onClose }: { onClose?: () => void }) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <button
        type="button"
        onClick={() => {
          setOpen(true);
        }}
      >
        Open
      </button>
      <InviteDialog
        open={open}
        onClose={() => {
          setOpen(false);
          onClose?.();
        }}
      />
    </>
  );
}

function renderOpener(onClose?: () => void) {
  return renderRoute({
    path: '/r/lounge',
    routes: [
      { path: '/r/:roomId', element: <Opener {...(onClose ? { onClose } : {})} /> },
      { path: '/admin/invites', element: <h1>Invites</h1> },
    ],
  });
}

const open = () => userEvent.click(screen.getByRole('button', { name: 'Open' }));
const dialog = () => screen.getByRole('dialog', { name: TITLE });

let dialogs: ReturnType<typeof stubNativeDialog>;

beforeEach(() => {
  dialogs = stubNativeDialog();
});

afterEach(() => {
  dialogs.restore();
});

describe('InviteDialog', () => {
  it('creates nothing while it is closed', async () => {
    const created = invites();
    renderOpener();
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(created).toEqual([]);
  });

  it("creates one invite with the server's defaults when it opens, and shows its link once in the invite card", async () => {
    const created = invites();
    renderOpener();
    await open();
    expect(within(dialog()).getByText(/Send this link to the friends you want to watch with/)).toBeInTheDocument();

    const link = await within(dialog()).findByRole('textbox', { name: 'Invite link' });
    expect(link).toHaveValue(linkOf(1));
    // setup/'s InviteLinkCard, with its texts: the lazy `setup` namespace is there.
    expect(within(dialog()).getByRole('button', { name: 'Copy' })).toBeInTheDocument();
    expect(within(dialog()).getByRole('img', { name: 'QR code of the invite link' })).toBeInTheDocument();
    expect(within(dialog()).getByText(/^Works for up to 10 people, until /)).toBeInTheDocument();
    expect(within(dialog()).getByText(/can't be shown again/)).toBeInTheDocument();
    expect(within(dialog()).getByRole('link', { name: 'Admin → Invites' })).toHaveAttribute('href', '/admin/invites');
    // Every field left to the server: 7 days and 10 uses unless an admin changed the defaults (03 §12.4.5).
    expect(created).toEqual([{}]);
  });

  it('says it is making the link until the server answers', async () => {
    let answer: () => void = () => undefined;
    const answered = new Promise<void>((resolve) => {
      answer = resolve;
    });
    server.use(
      http.post(apiPath('/api/v1/invites'), async () => {
        await answered;
        return HttpResponse.json(inviteResponse(1), { status: 201 });
      }),
    );
    renderOpener();
    await open();
    expect(await within(dialog()).findByText('Making an invite link…')).toBeInTheDocument();
    expect(within(dialog()).queryByRole('textbox')).not.toBeInTheDocument();
    answer();
    expect(await within(dialog()).findByRole('textbox', { name: 'Invite link' })).toHaveValue(linkOf(1));
    expect(within(dialog()).queryByText('Making an invite link…')).not.toBeInTheDocument();
  });

  it('puts the focus on the link once it is there, selected and ready to copy', async () => {
    invites();
    renderOpener();
    await open();
    const link = await within(dialog()).findByRole('textbox', { name: 'Invite link' });
    await waitFor(() => {
      expect(link).toHaveFocus();
    });
    const input = link as HTMLInputElement;
    expect([input.selectionStart, input.selectionEnd]).toEqual([0, linkOf(1).length]);
  });

  it('says why there is no link when the member has as many invites as allowed, and "Try again" makes one', async () => {
    const created = invites(apiError(409, { code: 'limit_reached', params: { limit: 'member_invites' } }), null);
    renderOpener();
    await open();
    expect(await within(dialog()).findByRole('alert')).toHaveTextContent(
      "Couldn't make an invite link. You have as many active invites as you're allowed. Revoke some first.",
    );
    expect(within(dialog()).queryByRole('textbox')).not.toBeInTheDocument();

    // Revoking one under My invites makes room: another try can work.
    await userEvent.click(within(dialog()).getByRole('button', { name: 'Try again' }));
    expect(await within(dialog()).findByRole('textbox', { name: 'Invite link' })).toHaveValue(linkOf(2));
    expect(within(dialog()).queryByRole('alert')).not.toBeInTheDocument();
    expect(created).toEqual([{}, {}]);
  });

  it('says why there is no link when the server fails, and "Try again" makes one', async () => {
    const created = invites(apiError(500, { code: 'internal' }), null);
    renderOpener();
    await open();
    expect(await within(dialog()).findByRole('alert')).toHaveTextContent(/^Couldn't make an invite link\./);
    await userEvent.click(within(dialog()).getByRole('button', { name: 'Try again' }));
    expect(await within(dialog()).findByRole('textbox', { name: 'Invite link' })).toHaveValue(linkOf(2));
    expect(created).toEqual([{}, {}]);
  });

  // 03 §7.9: no retry changes these. The menu offered the entry from a state that was out of date (a member gets
  // no admin.settings invalidation): the dialog marks it stale, so the menu reads it again when it opens next.
  it.each([
    [
      'registration was closed meanwhile',
      "['info']",
      apiError(403, { code: 'registration_closed' }),
      "Couldn't make an invite link. This server isn't taking new accounts.",
      queryKeys.info,
    ],
    [
      'the permission went meanwhile',
      "['me']",
      apiError(403, { code: 'forbidden' }),
      "Couldn't make an invite link. You're not allowed to do that.",
      queryKeys.me,
    ],
  ])(
    'says why there is no link when %s, offers only Done, and marks %s stale',
    async (_name, _key, failure, text, key) => {
      const created = invites(failure, null);
      const { services } = renderOpener();
      // ['me'] as the guard around the room has it (['info'] is seeded already).
      if (key === queryKeys.me) services.queryClient.setQueryData(queryKeys.me, meFixture({ createInvites: true }));
      expect(services.queryClient.getQueryState(key)?.isInvalidated).toBe(false);
      await open();
      expect(await within(dialog()).findByRole('alert')).toHaveTextContent(text);
      expect(within(dialog()).queryByRole('textbox')).not.toBeInTheDocument();
      expect(within(dialog()).queryByRole('button', { name: 'Try again' })).not.toBeInTheDocument();
      expect(within(dialog()).getByRole('button', { name: 'Done' })).toBeInTheDocument();
      await waitFor(() => {
        expect(services.queryClient.getQueryState(key)?.isInvalidated).toBe(true);
      });
      expect(created).toEqual([{}]);
    },
  );

  it('closes with Done and with ✕, and the next opening makes a new invite: the old link is not kept', async () => {
    const onClose = vi.fn();
    const created = invites();
    renderOpener(onClose);
    await open();
    expect(await within(dialog()).findByRole('textbox', { name: 'Invite link' })).toHaveValue(linkOf(1));

    await userEvent.click(within(dialog()).getByRole('button', { name: 'Done' }));
    expect(onClose).toHaveBeenCalledOnce();
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(screen.queryByDisplayValue(linkOf(1))).not.toBeInTheDocument();
    // Closed while in the page: the browser gives the focus back to what opened it.
    expect(dialogs.closes).toEqual([{ title: TITLE, inPage: true }]);

    await open();
    expect(await within(dialog()).findByRole('textbox', { name: 'Invite link' })).toHaveValue(linkOf(2));
    await userEvent.click(within(dialog()).getByRole('button', { name: 'Close' }));
    expect(onClose).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(created).toEqual([{}, {}]);
  });

  it('marks the invites list stale, so My invites and Admin → Invites show the new one', async () => {
    invites();
    const { services } = renderOpener();
    const list = [...queryKeys.invites, { state: 'all' }];
    services.queryClient.setQueryData(list, { invites: [] });
    await open();
    await within(dialog()).findByRole('textbox', { name: 'Invite link' });
    await waitFor(() => {
      expect(services.queryClient.getQueryState(list)?.isInvalidated).toBe(true);
    });
  });

  it('keeps the link out of the REST cache', async () => {
    invites();
    const { services } = renderOpener();
    await open();
    await within(dialog()).findByRole('textbox', { name: 'Invite link' });
    const cached = JSON.stringify(
      services.queryClient
        .getQueryCache()
        .getAll()
        .map((q) => q.state.data),
    );
    expect(cached).not.toContain(linkOf(1));
  });

  it('creates one invite under StrictMode too', async () => {
    const created = invites();
    const router = createMemoryRouter([{ path: '/', element: <InviteDialog open onClose={() => undefined} /> }]);
    render(
      <StrictMode>
        <AppProviders services={createTestServices()}>
          <RouterProvider router={router} />
        </AppProviders>
      </StrictMode>,
    );
    expect(await screen.findByRole('textbox', { name: 'Invite link' })).toHaveValue(linkOf(1));
    await act(() => new Promise((resolve) => setTimeout(resolve, 50)));
    expect(created).toHaveLength(1);
  });
});
