// desktop-reload.spec (M2 03 §22.4 A, §9, README WA65): the desktop page reloads while the app shares (a reload, or a
// webview that restarts): C2 keeps publishing, so the share goes on for its viewers without a gap, and the page that
// comes back adopts it (platform.sharing.current() → RoomSession.adoptShare): the panel is back and live, the own
// tile is local at the low layer again, and its Stop ends the share.
import { expect, test } from './fixtures.ts';
import { videoOf, waitForRoom, waitForState, waitForStats } from './stats.ts';

const LIVE_TIMEOUT_MS = 30_000;
const MEDIA_TIMEOUT_MS = 20_000;
const FAKE_LABEL = 'Fake window';

test.describe.configure({ timeout: 120_000 });

test('a reload while sharing: the share goes on for viewers, and the page adopts it', async ({
  startDesktop,
  server,
  asUser,
}) => {
  const room = 'lounge';
  const friend = await asUser(server);
  await friend.page.goto(`${server.url}/r/${room}`);
  await waitForRoom(friend.page, room);

  const account = await server.createUser();
  const desktop = await startDesktop({ server: server.url });
  const linked = await desktop.call<{ status: string }>('Auth', 'LinkWithPassword', account.username, account.password);
  expect(linked.status).toBe('linked');
  const { page } = desktop;
  await page.goto(`${desktop.origin}/r/${room}`);
  await waitForRoom(page, room);

  // Share from the page: the sheet, the fake pick, the first-share sheet, Go live.
  await page.getByRole('banner').getByRole('button', { name: 'Share', exact: true }).click();
  await page
    .getByRole('dialog', { name: 'Share your screen' })
    .getByRole('button', { name: 'Share', exact: true })
    .click();
  await page.getByRole('dialog', { name: 'Ready to go live?' }).getByRole('button', { name: 'Go live' }).click();
  const panel = page.getByRole('region', { name: 'Your share' });
  await expect(panel.getByText(`You're live · ${FAKE_LABEL}`)).toBeVisible({ timeout: LIVE_TIMEOUT_MS });

  const share = await waitForState(friend.page, (s) => s.viewer.shares.find((sh) => !sh.own && sh.status === 'live'), {
    timeout: LIVE_TIMEOUT_MS,
    message: "the desktop's share is live on the friend's page",
  });
  const before = await waitForStats(friend.page, (s) => (videoOf(s, share.id)?.framesDecoded ?? 0) > 10 && s, {
    timeout: MEDIA_TIMEOUT_MS,
    message: 'the friend decodes the share',
  });
  const framesBefore = videoOf(before, share.id)?.framesDecoded ?? 0;

  // The page reloads. C2 is the app's, not the page's: the share stays.
  await page.reload();
  await waitForRoom(page, room);

  // The page adopts it: the panel is back and live, and the Share button waits for this share to end.
  await expect(panel.getByText(`You're live · ${FAKE_LABEL}`)).toBeVisible({ timeout: LIVE_TIMEOUT_MS });
  await expect(page.getByRole('banner').getByRole('button', { name: 'Share', exact: true })).toBeDisabled();
  const own = await waitForState(
    page,
    (s) => s.viewer.shares.find((sh) => sh.id === share.id && sh.local && sh.subscription?.video === 'low'),
    { timeout: MEDIA_TIMEOUT_MS, message: 'the reloaded page counts the share as its own, at the low layer' },
  );
  expect(own.subscription?.audio).toBe('off');

  // For the friend it never stopped: the same share, still live, more frames.
  const current = await desktop.call<{ shareId: string; state: string } | null>('Share', 'Current');
  expect(current).toMatchObject({ shareId: share.id, state: 'live' });
  await waitForStats(friend.page, (s) => (videoOf(s, share.id)?.framesDecoded ?? 0) > framesBefore + 30 && s, {
    timeout: MEDIA_TIMEOUT_MS,
    message: 'the friend keeps decoding the same share',
  });
  const after = await waitForState(friend.page, (s) => s.viewer.shares.find((sh) => sh.id === share.id), {
    message: 'the share is still on the friend’s page',
  });
  expect(after.status).toBe('live');

  // The adopted share stops like one the page started.
  await panel.getByRole('button', { name: 'Stop sharing' }).click();
  await expect(panel).toBeHidden({ timeout: MEDIA_TIMEOUT_MS });
  await waitForState(friend.page, (s) => !s.viewer.shares.some((sh) => sh.id === share.id) && s, {
    timeout: MEDIA_TIMEOUT_MS,
    message: "the share is gone from the friend's page",
  });
  await expect(page.getByRole('banner').getByRole('button', { name: 'Share', exact: true })).toBeEnabled();
});
