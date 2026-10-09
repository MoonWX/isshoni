// invite.spec (05 §19.3; 05 §15.1, 03 §7.9): a friend opens an invite link in a browser of their own, signs up and
// lands in the Lounge, next to the person who invited them. The token of the link is in the URL's fragment, and
// the page takes it out of the address bar before it asks the server anything (05 §20).
//
// The worker's server is enough here: the test says only what also holds with other people in the lounge.
import { expect, test, uniqueName } from './fixtures.ts';
import { waitForRoom } from './stats.ts';

test('a friend opens an invite link, signs up and lands in the lounge with the inviter', async ({
  server,
  page,
  asUser,
  browser,
}) => {
  const admin = server.admin;
  if (!admin) throw new Error('the worker server has no admin');

  // The inviter is in the lounge already.
  const inviter = await asUser(server, admin);
  await inviter.page.goto(`${server.url}/`);
  await waitForRoom(inviter.page, 'lounge');
  const invite = await server.createInvite({ note: 'invite.spec', maxUses: 1 });
  expect(new URL(invite.url).hash).toBe(`#${invite.token}`);

  // The friend: this test's own context, which nobody signed in. Every URL its page asks for is kept.
  const requested: string[] = [];
  page.on('request', (request) => requested.push(request.url()));
  await page.goto(invite.url);

  // The page says who invited them, and the fragment is gone from the address bar.
  await expect(page.getByRole('heading', { name: new RegExp(`^${admin.username} invited you to `) })).toBeVisible();
  await expect(page).toHaveURL(`${server.url}/invite`);

  const username = uniqueName('guest');
  await page.getByLabel('Username').fill(username);
  await page.getByLabel('Password', { exact: true }).fill(`${uniqueName('correct-horse')}-staple`);
  await page.getByRole('button', { name: 'Create account' }).click();

  // Signed up and signed in: the default room, with no trace of the token in the URL …
  await expect(page).toHaveURL(`${server.url}/r/lounge`);
  await expect(page.getByRole('heading', { name: 'Lounge' })).toBeVisible();
  await waitForRoom(page, 'lounge');
  // … in any request (the token travels in JSON bodies only), or in what the tab keeps.
  expect(requested.length).toBeGreaterThan(0);
  expect(requested.filter((url) => url.includes(invite.token))).toEqual([]);
  const kept = await page.evaluate(() =>
    JSON.stringify([Object.entries(sessionStorage), Object.entries(localStorage)]),
  );
  expect(kept).not.toContain(invite.token);

  // Both appear in the people panel, on both pages.
  await page.getByRole('button', { name: /here\. Show who$/ }).click();
  const friendPanel = page.getByRole('dialog', { name: /people here$/ });
  await expect(friendPanel.getByText(`${username} (you)`)).toBeVisible();
  await expect(friendPanel.getByText(admin.username, { exact: true })).toBeVisible();

  await inviter.page.getByRole('button', { name: /here\. Show who$/ }).click();
  const inviterPanel = inviter.page.getByRole('dialog', { name: /people here$/ });
  await expect(inviterPanel.getByText(`${admin.username} (you)`)).toBeVisible();
  await expect(inviterPanel.getByText(username, { exact: true })).toBeVisible();

  // The link was good for one person: the next one to open it is told so, and gets no form.
  const late = await browser.newContext();
  try {
    const latePage = await late.newPage();
    await latePage.goto(invite.url);
    await expect(latePage.getByRole('heading', { name: "This invite link doesn't work" })).toBeVisible();
    await expect(latePage.getByText('This invite link has been used up. Ask for a new one.')).toBeVisible();
    await expect(latePage).toHaveURL(`${server.url}/invite`);
    await expect(latePage.getByLabel('Username')).toHaveCount(0);
  } finally {
    await late.close();
  }
});
