// rooms.spec (05 W5, README S66): presence in the Lounge, between browser contexts and the real server (01 §4.2,
// §4.3, §10.3):
// - two people see each other within a second of the second one's room.join;
// - closing a tab takes its person out of the room at once (the page closes its socket with 1000, and the hub
//   skips the grace), unless the person has another tab in the room;
// - a person whose network is cut stays, as "Reconnecting…", for the 30 s of the grace, is gone after it, and is
//   back by themselves when the network is;
// - the room switcher is there only once the server has a second room (05 §11.2), and switching takes the person
//   out of the room they leave, with their share, after the page has asked.
//
// Every test has a server of its own (startServer): these tests count the people of a room, and on the worker's
// server the people of other specs would be counted with them.
//
// The cut network is the harness's `link` (fixtures.ts), a SOCKS proxy between one browser context and the server,
// which the test closes: the server's end of the WebSocket goes away without a close frame, and no new connection
// gets through. Chrome's own offline emulation (context.setOffline) is no cut for a socket that is already open: it
// holds the frames back and leaves the TCP connection alone, so the server would notice nothing until its 45 s idle
// timeout.
import type { Locator, Page } from '@playwright/test';

import { enableDebug, expect, fakeDisplay, test } from './fixtures.ts';
import { readState, waitForRoom, waitForState } from './stats.ts';

/** 01 §13: how long a connection without a socket keeps its place. */
const GRACE_MS = 30_000;
/** "At once": what needs no grace period and no timeout shows within this, on a busy CI machine too. */
const AT_ONCE_MS = 3_000;

// ---- The page ----

/** The header's people count, "2 here". */
function peopleCount(page: Page): Locator {
  return page.getByRole('button', { name: /here\. Show who$/ });
}

/** Opens the people panel and returns it. */
async function openPeople(page: Page): Promise<Locator> {
  await peopleCount(page).click();
  const panel = page.getByRole('dialog', { name: /(person|people) here$/ });
  await expect(panel).toBeVisible();
  return panel;
}

/** A person's row in the people panel. */
function personRow(panel: Locator, userId: string): Locator {
  return panel.locator(`li[data-user-id="${userId}"]`);
}

/** The toasts: what a room.event says (05 §11.1). */
function toasts(page: Page): Locator {
  return page.locator('section[aria-label="Notifications"]');
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/** Notes a measured time in the test's report, next to the limit it was held against. */
function measured(what: string, ms: number): void {
  test.info().annotations.push({ type: 'measured', description: `${what}: ${String(Math.round(ms))} ms` });
}

// ---- The tests ----

test('two people in the lounge see each other within a second', async ({ startServer, context, page, asUser }) => {
  const server = await startServer();
  const admin = server.admin;
  if (!admin) throw new Error('the server has no admin');
  await server.signIn(context, admin);
  await page.goto(`${server.url}/`);
  await waitForRoom(page, 'lounge');
  await expect(peopleCount(page)).toHaveText('1 here');
  const adminPanel = await openPeople(page);
  await expect(personRow(adminPanel, admin.id)).toContainText(`${admin.username} (you)`);

  // The friend's page notes when it sends room.join and when its header first counts two people; the admin's page
  // notes when the friend's row appears. All three are Date.now() of the same machine.
  const friend = await asUser(server);
  await friend.page.addInitScript(() => {
    const marks: { joinSentAt?: number; sawTwoAt?: number } = {};
    (window as unknown as { __e2eMarks: typeof marks }).__e2eMarks = marks;
    // eslint-disable-next-line @typescript-eslint/unbound-method -- called below with the socket as its `this`
    const send = WebSocket.prototype.send;
    WebSocket.prototype.send = function (this: WebSocket, data) {
      if (typeof data === 'string' && data.includes('"room.join"')) marks.joinSentAt ??= Date.now();
      send.call(this, data);
    };
    new MutationObserver(() => {
      if (document.querySelector('button[aria-label^="2 here"]')) marks.sawTwoAt ??= Date.now();
    }).observe(document, { subtree: true, childList: true, attributes: true, attributeFilter: ['aria-label'] });
  });
  const adminSawFriendAt = page.evaluate(
    (userId) =>
      new Promise<number>((resolve) => {
        const observer = new MutationObserver(() => {
          if (document.querySelector(`li[data-user-id="${userId}"]`) === null) return;
          observer.disconnect();
          resolve(Date.now());
        });
        observer.observe(document.body, { subtree: true, childList: true });
      }),
    friend.account.id,
  );
  // Awaited below. A test that fails before it gets there ends with this evaluate still pending, and Playwright
  // would report its rejection ("page.evaluate: Test ended.") as a second error next to the real one.
  adminSawFriendAt.catch(() => undefined);

  await friend.page.goto(`${server.url}/`);
  const marks = await friend.page
    .waitForFunction(() => {
      const m = (window as unknown as { __e2eMarks?: { joinSentAt?: number; sawTwoAt?: number } }).__e2eMarks;
      return m?.joinSentAt !== undefined && m.sawTwoAt !== undefined ? { joined: m.joinSentAt, saw: m.sawTwoAt } : null;
    })
    .then((handle) => handle.jsonValue());
  if (marks === null) throw new Error("the friend's page has no marks");
  const friendSaw = marks.saw - marks.joined;
  const adminSaw = (await adminSawFriendAt) - marks.joined;
  measured('the friend counts two people after its room.join', friendSaw);
  measured("the admin sees the friend after the friend's room.join", adminSaw);
  expect(friendSaw, 'the friend counts two people after its room.join (ms)').toBeLessThanOrEqual(1000);
  expect(adminSaw, "the admin sees the friend after the friend's room.join (ms)").toBeLessThanOrEqual(1000);

  // And who they see: each other, by name. Everyone sees who is an admin.
  await expect(peopleCount(page)).toHaveText('2 here');
  await expect(adminPanel).toHaveAccessibleName('2 people here');
  await expect(personRow(adminPanel, friend.account.id)).toContainText(friend.account.username);
  await expect(toasts(page).getByText(`${friend.account.username} joined`)).toBeVisible();

  const friendPanel = await openPeople(friend.page);
  await expect(personRow(friendPanel, friend.account.id)).toContainText(`${friend.account.username} (you)`);
  await expect(personRow(friendPanel, admin.id)).toContainText(admin.username);
  await expect(personRow(friendPanel, admin.id)).toContainText('Admin');
  await expect(personRow(friendPanel, friend.account.id)).not.toContainText('Admin');
});

test('closing a tab takes its person out of the room at once', async ({ startServer, context, page, asUser }) => {
  const server = await startServer();
  const admin = server.admin;
  if (!admin) throw new Error('the server has no admin');
  await server.signIn(context, admin);
  await page.goto(`${server.url}/`);
  await waitForRoom(page, 'lounge');

  // The friend is in the room twice: two tabs of one browser.
  const friend = await asUser(server);
  await friend.page.goto(`${server.url}/`);
  await waitForRoom(friend.page, 'lounge');
  const secondTab = await friend.context.newPage();
  await secondTab.goto(`${server.url}/`);
  await waitForRoom(secondTab, 'lounge');

  // One person, however many tabs (01 §4.1).
  const panel = await openPeople(page);
  const row = personRow(panel, friend.account.id);
  await expect(row).toHaveCount(1);
  await expect(peopleCount(page)).toHaveText('2 here');

  // One tab closes: the person is still there through the other one.
  await secondTab.close();
  await sleep(1500);
  await expect(row).toHaveCount(1);
  await expect(row).not.toContainText('Reconnecting…');
  await expect(peopleCount(page)).toHaveText('2 here');

  // The last tab closes: gone at once, with no grace, and as someone who left.
  const closedAt = Date.now();
  await friend.page.close();
  await expect(row).toHaveCount(0, { timeout: AT_ONCE_MS });
  measured('the friend is out of the room after the tab began to close', Date.now() - closedAt);
  await expect(peopleCount(page)).toHaveText('1 here');
  await expect(toasts(page).getByText(`${friend.account.username} left`)).toBeVisible();
});

test('a person whose network is cut stays for the grace period, then is gone, and comes back with the network', async ({
  startServer,
  context,
  page,
  browser,
  link,
}) => {
  // The 30 s of the grace, and up to 12 s until the page's next try after it.
  test.setTimeout(120_000);
  const server = await startServer();
  const admin = server.admin;
  if (!admin) throw new Error('the server has no admin');
  await server.signIn(context, admin);
  await page.goto(`${server.url}/`);
  await waitForRoom(page, 'lounge');

  // The friend reaches the server through the link.
  const account = await server.createUser();
  const friendContext = await browser.newContext({ proxy: { server: link.proxy } });
  try {
    await enableDebug(friendContext);
    await server.signIn(friendContext, account);
    const friendPage = await friendContext.newPage();
    await friendPage.goto(`${server.url}/`);
    const before = await waitForRoom(friendPage, 'lounge');

    const panel = await openPeople(page);
    const row = personRow(panel, account.id);
    await expect(row).toContainText(account.username);
    await expect(row).not.toContainText('Reconnecting…');

    const cutAt = Date.now();
    link.cut();

    // The server lost the socket without a goodbye: the person stays, as reconnecting (01 §4.3).
    await expect(row).toContainText('Reconnecting…', { timeout: AT_ONCE_MS });
    measured('the friend shows as reconnecting after the cut', Date.now() - cutAt);
    await expect(peopleCount(page)).toHaveText('2 here');
    // The friend's page knows too, and keeps its room while it tries again.
    await waitForState(friendPage, (s) => s.connection.state !== 'ready', {
      message: "the friend's page lost the server",
    });
    await expect(friendPage.getByTestId('connection-banner')).toContainText('Reconnecting…');
    expect((await readState(friendPage)).room.roomId).toBe('lounge');

    // Through the grace: still there, a second at a time, until 3 s before its end.
    while (Date.now() - cutAt < GRACE_MS - 3_000) {
      await expect(row, `the friend's row ${String(Date.now() - cutAt)} ms after the cut`).toContainText(
        'Reconnecting…',
      );
      await sleep(1_000);
    }
    // After it: gone, as someone who was disconnected.
    await expect(row).toHaveCount(0, { timeout: 3_000 + AT_ONCE_MS });
    measured('the friend is out of the room after the cut', Date.now() - cutAt);
    expect(Date.now() - cutAt, 'the friend left the room after (ms)').toBeGreaterThanOrEqual(GRACE_MS - 1_000);
    await expect(peopleCount(page)).toHaveText('1 here');
    await expect(toasts(page).getByText(`${account.username} disconnected`)).toBeVisible();

    // The network is back: the page connects by itself (its tries are at most 12 s apart, 01 §10.2), as a new
    // connection, since the old one is gone, and joins its room again.
    link.restore();
    const after = await waitForState(
      friendPage,
      (s) => s.connection.state === 'ready' && s.room.joinState === 'joined' && s.room.participants === 2 && s,
      { timeout: 20_000, message: "the friend's page is back in the lounge" },
    );
    expect(after.connection.resumed).toBe(false);
    expect(after.connection.connectionId).not.toBe(before.connection.connectionId);
    await expect(row).toContainText(account.username, { timeout: 5_000 });
    await expect(row).not.toContainText('Reconnecting…');
    await expect(peopleCount(page)).toHaveText('2 here');
    await expect(friendPage.getByTestId('connection-banner')).toHaveText('');
  } finally {
    await friendContext.close();
  }
});

test('the room switcher comes with a second room, and switching rooms takes the person and their share along', async ({
  startServer,
  context,
  page,
  asUser,
}) => {
  const server = await startServer();
  const admin = server.admin;
  if (!admin) throw new Error('the server has no admin');
  await server.signIn(context, admin);
  // What the admin shares, in a headful run too: the fake display needs no picker and no second tab (05 §19.3).
  await fakeDisplay(page, 'window+audio');
  await page.goto(`${server.url}/`);
  await waitForRoom(page, 'lounge');
  const friend = await asUser(server);
  await friend.page.goto(`${server.url}/`);
  await waitForRoom(friend.page, 'lounge');

  // One room: nothing to switch to, so no switcher.
  const switcher = (p: Page): Locator => p.getByRole('button', { name: 'Switch room' });
  await expect(peopleCount(page)).toHaveText('2 here');
  await expect(switcher(page)).toHaveCount(0);
  await expect(switcher(friend.page)).toHaveCount(0);

  // The admin makes a second room (the REST call of Admin → Rooms): both pages hear of it and get the switcher.
  const created = await context.request.post(`${server.url}/api/v1/admin/rooms`, { data: { name: 'Cinema' } });
  expect(created.status()).toBe(201);
  const cinema = ((await created.json()) as { room: { id: string } }).room.id;
  await expect(switcher(page)).toBeVisible();
  await expect(switcher(friend.page)).toBeVisible();
  await switcher(friend.page).click();
  const friendRooms = friend.page.getByRole('dialog', { name: 'Rooms' });
  await expect(friendRooms.getByRole('listitem').filter({ hasText: 'Lounge' })).toContainText('2 here');
  await expect(friendRooms.getByRole('link', { name: /^Cinema/ })).toContainText('0 here');
  // "Create room" is the admin's.
  await expect(friendRooms.getByRole('link', { name: 'Create room' })).toHaveCount(0);
  await friend.page.keyboard.press('Escape');

  // The admin shares, and the friend watches.
  await page.getByRole('banner').getByRole('button', { name: 'Share', exact: true }).click();
  await page
    .getByRole('dialog', { name: 'Share your screen' })
    .getByRole('button', { name: 'Share', exact: true })
    .click();
  await waitForState(page, (s) => s.viewer.shares.some((share) => share.local && share.status === 'live'), {
    timeout: 20_000,
    message: "the admin's share is live",
  });
  await expect(friend.page.getByRole('region', { name: `Now watching: ${admin.username}'s window` })).toBeVisible();

  // Leaving the room would end the share, so the switcher asks first. Cancel: nothing changes.
  await switcher(page).click();
  const adminRooms = page.getByRole('dialog', { name: 'Rooms' });
  await expect(adminRooms.getByRole('link', { name: 'Create room' })).toBeVisible();
  await adminRooms.getByRole('link', { name: /^Cinema/ }).click();
  const question = page.getByRole('dialog', { name: 'Stop sharing and switch to Cinema?' });
  await expect(question).toBeVisible();
  await question.getByRole('button', { name: 'Cancel' }).click();
  await expect(question).toBeHidden();
  await expect(page).toHaveURL(`${server.url}/r/lounge`);
  expect((await readState(page)).viewer.shares).toHaveLength(1);

  // Confirmed: the admin is in the other room, alone, without a share; the lounge has neither of them anymore.
  await switcher(page).click();
  await adminRooms.getByRole('link', { name: /^Cinema/ }).click();
  await question.getByRole('button', { name: 'Stop sharing and switch' }).click();
  await expect(page).toHaveURL(`${server.url}/r/${cinema}`);
  await expect(page.getByRole('heading', { name: 'Cinema' })).toBeVisible();
  const moved = await waitForRoom(page, cinema);
  expect(moved.room.participants).toBe(1);
  expect(moved.viewer.shares).toEqual([]);
  await expect(page.getByRole('region', { name: 'Your share' })).toHaveCount(0);
  await expect(peopleCount(page)).toHaveText('1 here');

  await waitForState(friend.page, (s) => s.room.participants === 1 && s.viewer.shares.length === 0, {
    timeout: AT_ONCE_MS,
    message: 'the lounge without the admin and the share',
  });
  await expect(peopleCount(friend.page)).toHaveText('1 here');
  await expect(toasts(friend.page).getByText(`${admin.username} left`)).toBeVisible();
  await expect(friend.page.getByText('Nobody is sharing yet.')).toBeVisible();

  // The friend follows: no question for someone who shares nothing, and the two are together again.
  await switcher(friend.page).click();
  await expect(friendRooms.getByRole('link', { name: /^Cinema/ })).toContainText('1 here');
  await friendRooms.getByRole('link', { name: /^Cinema/ }).click();
  await expect(friend.page).toHaveURL(`${server.url}/r/${cinema}`);
  await waitForRoom(friend.page, cinema);
  await expect(peopleCount(friend.page)).toHaveText('2 here');
  await expect(peopleCount(page)).toHaveText('2 here');
  const panel = await openPeople(page);
  await expect(personRow(panel, friend.account.id)).toContainText(friend.account.username);
});
