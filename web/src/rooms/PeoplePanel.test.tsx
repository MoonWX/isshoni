// The people panel (05 §11.2): everyone in the room with a "Sharing" badge and the admin badge, reconnecting
// people dimmed, and a click on a person who shares focuses their share.
import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { AppProviders } from '../app/App';
import type { RoomState } from '../protocol/types.gen';
import { renderWithApp } from '../test/render';
import { PeoplePanel, type PeoplePanelProps } from './PeoplePanel';
import { participant, shareInfo } from './testing/harness';

const alex = participant('u_alex', 'alex');
const bo = participant('u_bo', 'bo');
const cy = participant('u_cy', 'cy');

// The Go side's golden room.state fixtures (01 §14.3), as protocol/registry.test.ts imports them: what the hub
// encodes is what the panel reads.
const goldenFiles = import.meta.glob<{ data: RoomState }>('../../../internal/protocol/testdata/v1/room.state*.json', {
  eager: true,
  import: 'default',
});

function golden(name: string): RoomState {
  const file = Object.entries(goldenFiles).find(([path]) => path.endsWith(`/${name}.json`));
  if (file === undefined) throw new Error(`no fixture ${name}.json`);
  return file[1].data;
}

function renderPanel(props: Partial<PeoplePanelProps> = {}) {
  const onWatch = vi.fn();
  const onClose = vi.fn();
  const panel = (more: Partial<PeoplePanelProps> = {}) => (
    <PeoplePanel
      open
      onClose={onClose}
      participants={[alex, bo, cy]}
      shares={[]}
      selfUserId="u_alex"
      onWatch={onWatch}
      {...props}
      {...more}
    />
  );
  const { rerender, services } = renderWithApp(panel());
  /** The same panel with other props, as after the next room.state. */
  const update = (more: Partial<PeoplePanelProps>) => {
    rerender(<AppProviders services={services}>{panel(more)}</AppProviders>);
  };
  return { onWatch, onClose, update };
}

/** A person's row, by their user id. */
function row(userId: string): HTMLElement {
  const li = document.querySelector<HTMLElement>(`li[data-user-id="${userId}"]`);
  if (li === null) throw new Error(`no row for ${userId}`);
  return li;
}

describe('PeoplePanel', () => {
  it('is a drawer that lists everyone, and marks the user’s own row', () => {
    renderPanel();
    const panel = screen.getByRole('dialog', { name: '3 people here' });
    expect(within(panel).getAllByRole('listitem')).toHaveLength(3);
    expect(row('u_alex')).toHaveTextContent('alex (you)');
    expect(row('u_bo')).toHaveTextContent('bo');
    expect(row('u_bo')).not.toHaveTextContent('(you)');
  });

  it('lists nobody while it is closed (the page keeps it mounted, 05 §16.6)', () => {
    renderPanel({ open: false });
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(document.querySelectorAll('li[data-user-id]')).toHaveLength(0);
    expect(screen.queryByText('alex (you)')).not.toBeInTheDocument();
  });

  it('counts one person as one', () => {
    renderPanel({ participants: [alex] });
    expect(screen.getByRole('dialog', { name: '1 person here' })).toBeInTheDocument();
  });

  it('says so when nobody is listed', () => {
    renderPanel({ participants: [] });
    expect(screen.getByText('Nobody is here yet.')).toBeInTheDocument();
  });

  it('gives a person who shares the "Sharing" badge', () => {
    renderPanel({ shares: [shareInfo('s_bo', 'u_bo', 'c_bo')] });
    expect(within(row('u_bo')).getByText('Sharing')).toBeInTheDocument();
    expect(within(row('u_alex')).queryByText('Sharing')).not.toBeInTheDocument();
    expect(within(row('u_cy')).queryByText('Sharing')).not.toBeInTheDocument();
  });

  it('shows the admin badge on every admin, the user’s own row or not, and on no member', () => {
    renderPanel({
      participants: [participant('u_alex', 'alex', { admin: true }), participant('u_bo', 'bo', { admin: true }), cy],
    });
    expect(within(row('u_alex')).getByText('Admin')).toBeInTheDocument();
    expect(within(row('u_bo')).getByText('Admin')).toBeInTheDocument();
    expect(within(row('u_cy')).queryByText('Admin')).not.toBeInTheDocument();
    expect(screen.getAllByText('Admin')).toHaveLength(2);
  });

  it('shows another person’s admin badge to a member', () => {
    renderPanel({ participants: [alex, participant('u_bo', 'bo', { admin: true })] });
    expect(within(row('u_bo')).getByText('Admin')).toBeInTheDocument();
    expect(within(row('u_alex')).queryByText('Admin')).not.toBeInTheDocument();
  });

  it('shows no admin badge when nobody is an admin: the flag is absent, or false', () => {
    renderPanel({ participants: [alex, participant('u_bo', 'bo', { admin: false }), cy] });
    expect(screen.queryByText('Admin')).not.toBeInTheDocument();
  });

  it('the admin badge follows the role with the next room.state', () => {
    const { update } = renderPanel();
    expect(screen.queryByText('Admin')).not.toBeInTheDocument();
    update({ participants: [alex, participant('u_bo', 'bo', { admin: true }), cy] });
    expect(within(row('u_bo')).getByText('Admin')).toBeInTheDocument();
    update({ participants: [alex, bo, cy] });
    expect(screen.queryByText('Admin')).not.toBeInTheDocument();
  });

  it('an admin who shares has both badges, and is still a button for their share', async () => {
    const { onWatch } = renderPanel({
      participants: [alex, participant('u_bo', 'bo', { admin: true })],
      shares: [shareInfo('s_bo', 'u_bo', 'c_bo')],
    });
    const button = within(row('u_bo')).getByRole('button', { name: /bo/ });
    expect(within(button).getByText('Admin')).toBeInTheDocument();
    expect(within(button).getByText('Sharing')).toBeInTheDocument();
    await userEvent.click(button);
    expect(onWatch).toHaveBeenCalledExactlyOnceWith('s_bo');
  });

  it('an admin who is reconnecting keeps the badge', () => {
    renderPanel({ participants: [alex, participant('u_bo', 'bo', { admin: true, status: 'reconnecting' })] });
    expect(within(row('u_bo')).getByText('Admin')).toBeInTheDocument();
    expect(within(row('u_bo')).getByText('Reconnecting…')).toBeInTheDocument();
  });

  it('reads the flag as the hub encodes it (the golden room.state.admin.json)', () => {
    const state = golden('room.state.admin');
    renderPanel({ participants: state.participants, shares: state.shares, selfUserId: 'b8f2n4r6t0vz' });
    // Alex is the admin of the fixture; Bea, this user, is a member.
    expect(within(row('k3m9p2qxw7ht')).getByText('Admin')).toBeInTheDocument();
    expect(row('b8f2n4r6t0vz')).toHaveTextContent('Bea (you)');
    expect(screen.getAllByText('Admin')).toHaveLength(1);
  });

  it('a room.state from before the flag shows everyone, and no admin badge', () => {
    const state = golden('room.state');
    renderPanel({ participants: state.participants, shares: state.shares, selfUserId: 'k3m9p2qxw7ht' });
    expect(document.querySelectorAll('li[data-user-id]')).toHaveLength(2);
    expect(screen.queryByText('Admin')).not.toBeInTheDocument();
  });

  it('dims a person who is reconnecting, and says it in words too', () => {
    renderPanel({ participants: [alex, participant('u_bo', 'bo', { status: 'reconnecting' })] });
    expect(within(row('u_bo')).getByText('Reconnecting…')).toBeInTheDocument();
    expect(row('u_bo').className).toMatch(/dim/);
    expect(row('u_alex').className).not.toMatch(/dim/);
    expect(within(row('u_alex')).queryByText('Reconnecting…')).not.toBeInTheDocument();
  });

  it('a click on a person who shares focuses their share and closes the panel', async () => {
    const { onWatch, onClose } = renderPanel({ shares: [shareInfo('s_bo', 'u_bo', 'c_bo')] });
    const button = within(row('u_bo')).getByRole('button', { name: /bo/ });
    expect(button).toHaveAccessibleDescription("Watch bo's share");
    await userEvent.click(button);
    expect(onWatch).toHaveBeenCalledExactlyOnceWith('s_bo');
    expect(onClose).toHaveBeenCalledOnce();
  });

  it('focuses the newest of a person’s shares', async () => {
    const { onWatch } = renderPanel({
      shares: [
        shareInfo('s_new', 'u_bo', 'c_bo', { startedAt: '2026-10-12T19:09:00.000Z' }),
        shareInfo('s_old', 'u_bo', 'c_bo2', { startedAt: '2026-10-12T19:01:00.000Z' }),
      ],
    });
    await userEvent.click(within(row('u_bo')).getByRole('button'));
    expect(onWatch).toHaveBeenCalledExactlyOnceWith('s_new');
  });

  it('the user’s own share can be put on the stage too', async () => {
    const { onWatch } = renderPanel({ shares: [shareInfo('s_mine', 'u_alex', 'c_alex')] });
    const button = within(row('u_alex')).getByRole('button');
    expect(button).toHaveAccessibleDescription('Show your share on the stage');
    await userEvent.click(button);
    expect(onWatch).toHaveBeenCalledExactlyOnceWith('s_mine');
  });

  it('people who don’t share are not buttons', () => {
    renderPanel({ shares: [shareInfo('s_bo', 'u_bo', 'c_bo')] });
    expect(within(row('u_cy')).queryByRole('button')).not.toBeInTheDocument();
  });

  it('a share that is still starting has the badge but nothing to watch yet (01 §4.4)', () => {
    renderPanel({ shares: [shareInfo('s_bo', 'u_bo', 'c_bo', { status: 'starting' })] });
    expect(within(row('u_bo')).getByText('Sharing')).toBeInTheDocument();
    expect(within(row('u_bo')).queryByRole('button')).not.toBeInTheDocument();
  });

  it('shows nothing about other people’s devices (01 §8.5)', () => {
    const phone = participant('u_bo', 'bo', {
      connections: [
        { id: 'c_bo_1', kind: 'web', role: 'viewer', status: 'online' },
        { id: 'c_bo_2', kind: 'desktop', role: 'full', status: 'online' },
      ],
    });
    renderPanel({ participants: [alex, phone] });
    expect(row('u_bo').textContent).toBe('Bbo');
  });

  it('the avatar’s initial is a whole character', () => {
    renderPanel({ participants: [participant('u_k', '太郎'), participant('u_e', '🎬 movies')] });
    expect(row('u_k')).toHaveTextContent(/^太太郎$/);
    expect(row('u_e').textContent).toBe('🎬🎬 movies');
  });
});
