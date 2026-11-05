// The mic prompt card in the window's corner (docs/m2/03 §10.3): while no share panel is mounted (the user is on
// Settings or another page with the window in front, so the Go side sends no toast), a prompt shows in a non-modal
// corner region with its buttons; while a panel shows the prompts, the corner card steps aside, so the question is
// on screen once; the panel's unmount brings it back.
import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { describe, expect, it } from 'vitest';

import type { FriendsHear } from '../../platform/desktop/bridge';
import { shareView } from '../../platform/desktop/testing/shareView';
import { createTestServices, renderRoute } from '../../test/render';
import { MicPromptCorner } from './MicPromptCorner';
import { NativeSharePanel } from './NativeSharePanel';
import { friendsHear, nativeSetup, type NativeSetup } from './testing';

const KOOK = { id: 'p1', appId: 'exe:kook.exe', name: 'KOOK', kind: 'mic', since: 1 } as const;
const question = 'KOOK is using your microphone. Keep it out of your stream?';

async function renderCorner(fh: FriendsHear, withPanel: boolean): Promise<NativeSetup> {
  const setup = await nativeSetup({ current: shareView() });
  setup.fake.answers['audio.friendsHear'] = fh;
  function Page() {
    const [panel, setPanel] = useState(withPanel);
    return (
      <>
        <button
          type="button"
          onClick={() => {
            setPanel((p) => !p);
          }}
        >
          toggle panel
        </button>
        {panel && <NativeSharePanel share={setup.sharing.current()} onStop={() => undefined} store={setup.store} />}
        <MicPromptCorner />
      </>
    );
  }
  renderRoute({
    services: createTestServices({ platform: setup.platform }),
    routes: [{ path: '/', element: <Page /> }],
  });
  return setup;
}

const corner = () => screen.queryByRole('region', { name: 'Microphone questions' });

describe('MicPromptCorner', () => {
  it('shows a prompt in the corner while no panel is mounted; its buttons call decide', async () => {
    const user = userEvent.setup();
    const setup = await renderCorner(friendsHear({ prompts: [KOOK] }), false);
    const region = await screen.findByRole('region', { name: 'Microphone questions' });
    const card = within(region).getByRole('group', { name: question });
    expect(within(card).getByRole('button', { name: 'Include' })).toBeInTheDocument();
    await user.click(within(card).getByRole('button', { name: 'Keep out' }));
    expect(setup.fake.callsOf('audio.decide')).toEqual([['p1', 'keep-out']]);
  });

  it('shows nothing without a prompt, and a prompt that comes later', async () => {
    const setup = await renderCorner(friendsHear({ seq: 1 }), false);
    await waitFor(() => {
      expect(setup.fake.callsOf('audio.friendsHear')).toHaveLength(1);
    });
    expect(corner()).not.toBeInTheDocument();
    act(() => {
      setup.fake.emit('audio.report', friendsHear({ seq: 2, prompts: [KOOK] }));
    });
    expect(await screen.findByRole('region', { name: 'Microphone questions' })).toBeInTheDocument();
  });

  it('steps aside while a panel shows the prompts, and comes back when the panel goes', async () => {
    const user = userEvent.setup();
    await renderCorner(friendsHear({ prompts: [KOOK] }), true);
    // The panel's card, once.
    expect(await screen.findByRole('group', { name: question })).toBeInTheDocument();
    expect(screen.getAllByRole('group', { name: question })).toHaveLength(1);
    expect(corner()).not.toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'toggle panel' }));
    const region = await screen.findByRole('region', { name: 'Microphone questions' });
    expect(within(region).getByRole('group', { name: question })).toBeInTheDocument();
    expect(screen.getAllByRole('group', { name: question })).toHaveLength(1);
  });
});
