// The share sheet under every scenario of 05's fake engine (ISSHONI_FAKE_AUDIO, 05 §13.3; docs/m2/03 DK8, README
// WA77): one snapshot each. The fixtures (testdata/friendsHear/<scenario>.json) are what Audio.FriendsHear() answers
// with the real 05 service on the fake, 6 s after the sheet started watching; internal/desktop/bridge's
// TestFakeScenarioFixtures keeps them equal to it (go test -run TestFakeScenarioFixtures -update-scenarios writes
// them). The sheet is snapshotted with its Details open, so every group and row is in it, and the first-share sheet
// with its groups too: testdata/sheets/<scenario>.{share,first-share}.txt, one element per line (`vitest -u` writes
// them again).
import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import type { FriendsHear } from '../../platform/desktop/bridge';
import { createTestServices, renderRoute } from '../../test/render';
import { NativeShareButton } from './NativeShareButton';
import { nativeSetup, PICKED } from './testing';

const fixtures = import.meta.glob<FriendsHear>('./testdata/friendsHear/*.json', { eager: true, import: 'default' });

/** The scenarios of internal/media/fake (audioScenarios). */
const SCENARIOS = [
  'normal',
  'degraded',
  'degrade-live',
  'old-windows',
  'no-device',
  'fast-path',
  'shared-mic',
  'many',
] as const;

function fixture(name: string): FriendsHear {
  const fh = fixtures[`./testdata/friendsHear/${name}.json`];
  if (fh === undefined) throw new Error(`no fixture for ${name}`);
  return fh;
}

/**
 * The sheet's markup without React's generated ids, which change with the order the tests run in, and with the CSS
 * modules' class names without their hashes (`_name_1b6567` → `name`), which follow the files' contents.
 */
function markup(el: HTMLElement): string {
  return `${el.outerHTML
    .replace(/_r_[0-9a-z]+_|«r[0-9a-z]+»|:r[0-9a-z]+:/g, 'ID')
    .replace(/\b_([A-Za-z][A-Za-z0-9]*)_[0-9a-f]{6}\b/g, '$1')
    .replaceAll('><', '>\n<')}\n`;
}

describe('the share sheet under every fake audio scenario (05 §13.3)', () => {
  it('has one fixture per scenario', () => {
    expect(Object.keys(fixtures).sort()).toEqual(SCENARIOS.map((s) => `./testdata/friendsHear/${s}.json`).sort());
  });

  it.each(SCENARIOS)('%s', async (name) => {
    const user = userEvent.setup();
    const fh = fixture(name);
    const setup = await nativeSetup({ picked: { ...PICKED, confirm: ['first-share'] } });
    setup.fake.answers['audio.friendsHear'] = fh;
    renderRoute({
      services: createTestServices({ platform: setup.platform }),
      routes: [{ path: '/', element: <NativeShareButton onStart={() => Promise.resolve()} store={setup.store} /> }],
    });
    await user.click(screen.getByRole('button', { name: 'Share' }));
    const sheet = screen.getByRole('dialog', { name: 'Share your screen' });
    const details = await within(sheet).findByRole('button', { name: 'Details' });
    await user.click(details);
    if (fh.engine !== 'endpoint' && fh.heard.length > 0) {
      expect(within(sheet).getByRole('region', { name: 'Friends will hear:' })).toBeInTheDocument();
    }
    await expect(markup(sheet)).toMatchFileSnapshot(`./testdata/sheets/${name}.share.txt`);

    await user.click(within(sheet).getByRole('button', { name: 'Share' }));
    const first = await screen.findByRole('dialog', { name: 'Ready to go live?' });
    await within(first).findByText("Muting your PC doesn't mute your stream. Use Pause or turn Share sound off.");
    await expect(markup(first)).toMatchFileSnapshot(`./testdata/sheets/${name}.first-share.txt`);
  });
});
