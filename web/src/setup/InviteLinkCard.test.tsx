// InviteLinkCard (05 §14.1): the link once, with Copy, the QR code, Share… where the browser has it, the expiry and
// the uses, and where to make another link: under Admin → Invites, or where the caller's own text says (onceKey).
import { act, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { formatDateTime } from '../lib/time';
import type { CreateInviteResponse } from '../protocol/api.gen';
import { infoFixture } from '../test/msw';
import { createTestServices, renderRoute } from '../test/render';
import { InviteLinkCard } from './InviteLinkCard';

const URL = 'https://watch.example.com/invite#EXAMPLEinviteTOKEN0123456789abcd';
const EXPIRES = '2026-10-08T12:00:00.000Z';

function created(overrides: Partial<CreateInviteResponse['invite']> = {}): CreateInviteResponse {
  return {
    url: URL,
    invite: {
      id: 'h6j8k0m2n4p6',
      note: '',
      createdBy: { id: 'a1b2c3d4e5f6', username: 'admin' },
      createdAt: '2026-10-01T12:00:00.000Z',
      expiresAt: EXPIRES,
      maxUses: 10,
      uses: 0,
      state: 'active',
      redeemedBy: [],
      ...overrides,
    },
  };
}

function renderCard(response: CreateInviteResponse = created(), onceKey?: string) {
  const services = createTestServices({
    info: infoFixture({ server: { name: 'Movie night', version: '0.1.0', publicUrl: '' } }),
  });
  return renderRoute({
    routes: [{ path: '/', element: <InviteLinkCard created={response} onceKey={onceKey} /> }],
    services,
  });
}

function stubNavigator(name: 'clipboard' | 'share', value: unknown): void {
  Object.defineProperty(navigator, name, { configurable: true, value });
}

afterEach(() => {
  Reflect.deleteProperty(navigator, 'clipboard');
  Reflect.deleteProperty(navigator, 'share');
  vi.useRealTimers();
});

describe('InviteLinkCard', () => {
  it('shows the link, its QR code, how long and for how many it works, and that it is shown once', () => {
    renderCard();
    const field = screen.getByRole('textbox', { name: 'Invite link' });
    expect(field).toHaveValue(URL);
    expect(field).toHaveAttribute('readonly');
    expect(screen.getByRole('img', { name: 'QR code of the invite link' })).toBeInTheDocument();
    expect(screen.getByText("Scan it with a phone's camera.")).toBeInTheDocument();
    const date = formatDateTime(new Date(EXPIRES), 'en');
    expect(screen.getByText(`Works for up to 10 people, until ${date}.`)).toBeInTheDocument();
    expect(screen.getByText(/Copy it now: for safety it can't be shown again/)).toBeInTheDocument();
  });

  // The wizard's step 3 gives no onceKey: the admin's menu has Admin, and Invites under it.
  it('says where to make another link: under Admin → Invites by default', () => {
    renderCard();
    const link = screen.getByRole('link', { name: 'Admin → Invites' });
    expect(link).toHaveAttribute('href', '/admin/invites');
    expect(link.closest('p')).toHaveTextContent(
      "Copy it now: for safety it can't be shown again. You can always make a new one under Admin → Invites.",
    );
  });

  // The room's invite dialog for a member: their menu has no Admin, but My invites, a link to the same page.
  it("says the caller's text when given onceKey, its link still to /admin/invites", () => {
    renderCard(created(), 'room.invite.once');
    const link = screen.getByRole('link', { name: 'My invites' });
    expect(link).toHaveAttribute('href', '/admin/invites');
    expect(link.closest('p')).toHaveTextContent(
      "Copy it now: for safety it can't be shown again. You'll find the invite under My invites in your account " +
        'menu, where you can always make a new one.',
    );
    expect(screen.queryByText(/Admin → Invites/)).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: 'Admin → Invites' })).not.toBeInTheDocument();
  });

  it('says "1 person" for a one-use invite', () => {
    renderCard(created({ maxUses: 1 }));
    const date = formatDateTime(new Date(EXPIRES), 'en');
    expect(screen.getByText(`Works for 1 person, until ${date}.`)).toBeInTheDocument();
  });

  it('copies the link on a click and says so', async () => {
    const writeText = vi.fn(() => Promise.resolve());
    stubNavigator('clipboard', { writeText });
    const { services } = renderCard();
    await userEvent.click(screen.getByRole('button', { name: 'Copy' }));
    expect(writeText).toHaveBeenCalledWith(URL);
    expect(await screen.findByRole('button', { name: 'Copied' })).toBeInTheDocument();
    expect(services.ui.getState().announcements.polite?.text).toBe('Invite link copied');
    expect(screen.getByRole('status')).toBeEmptyDOMElement();
  });

  it('goes back to "Copy" after 2 seconds', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    stubNavigator('clipboard', { writeText: () => Promise.resolve() });
    renderCard();
    const user = userEvent.setup({ advanceTimers: (ms) => vi.advanceTimersByTime(ms) });
    await user.click(screen.getByRole('button', { name: 'Copy' }));
    expect(await screen.findByRole('button', { name: 'Copied' })).toBeInTheDocument();
    act(() => {
      vi.advanceTimersByTime(2000);
    });
    expect(screen.getByRole('button', { name: 'Copy' })).toBeInTheDocument();
  });

  it.each([
    ['refuses', { writeText: () => Promise.reject(new DOMException('denied', 'NotAllowedError')) }],
    ['has no clipboard', undefined],
  ])('selects the link to copy by hand when the browser %s', async (_name, clipboard) => {
    if (clipboard) stubNavigator('clipboard', clipboard);
    renderCard();
    await userEvent.click(screen.getByRole('button', { name: 'Copy' }));
    expect(await screen.findByRole('status')).toHaveTextContent(
      "Couldn't copy for you. The link is selected: copy it yourself.",
    );
    const field = screen.getByRole('textbox', { name: 'Invite link' });
    expect(field).toHaveFocus();
    expect((field as HTMLInputElement).selectionStart).toBe(0);
    expect((field as HTMLInputElement).selectionEnd).toBe(URL.length);
  });

  it('has no Share button where the browser has no share sheet', () => {
    renderCard();
    expect(screen.queryByRole('button', { name: 'Share…' })).not.toBeInTheDocument();
  });

  it('opens the share sheet with the link and the server’s name', async () => {
    const share = vi.fn(() => Promise.resolve());
    stubNavigator('share', share);
    renderCard();
    await userEvent.click(screen.getByRole('button', { name: 'Share…' }));
    expect(share).toHaveBeenCalledWith({
      title: 'Join me on Movie night',
      text: 'Watch screens together with me on Movie night.',
      url: URL,
    });
    expect(screen.getByRole('status')).toBeEmptyDOMElement();
  });

  it('says nothing when the user closes the share sheet', async () => {
    const share = vi.fn(() => Promise.reject(new DOMException('cancelled', 'AbortError')));
    stubNavigator('share', share);
    renderCard();
    await userEvent.click(screen.getByRole('button', { name: 'Share…' }));
    await waitFor(() => {
      expect(share).toHaveBeenCalledTimes(1);
    });
    expect(screen.getByRole('status')).toBeEmptyDOMElement();
    expect(screen.getByRole('textbox', { name: 'Invite link' })).not.toHaveFocus();
  });

  it('selects the link when the share sheet fails', async () => {
    stubNavigator('share', () => Promise.reject(new DOMException('not allowed', 'NotAllowedError')));
    renderCard();
    await userEvent.click(screen.getByRole('button', { name: 'Share…' }));
    expect(await screen.findByRole('status')).toHaveTextContent(
      "Couldn't open sharing. The link is selected: copy it instead.",
    );
    expect(screen.getByRole('textbox', { name: 'Invite link' })).toHaveFocus();
  });
});
