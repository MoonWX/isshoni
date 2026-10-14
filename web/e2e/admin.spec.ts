// admin.spec (05 §19.3, §15.3): the admin pages against a real server. An admin creates an invite link and revokes
// it, approves a pending sign-up in approval mode, and runs doctor, whose page renders the server's results from the
// catalog: on an e2e server (off mode, loopback candidates, no STUN) the public_ip check is skipped with its own
// text (README F04). The dashboard shows the live server and asks for a new snapshot every 2 s.
//
// Every test has a server of its own: approval mode is server-wide, and the invite and doctor tests count what the
// server lists.
import type { Locator, Page, Request, Response } from '@playwright/test';

import { enableDebug, expect, test, uniqueName, type Account, type E2EServer } from './fixtures.ts';
import { waitForRoom } from './stats.ts';

/** Opens an admin page signed in as the server's admin. */
async function openAdmin(server: E2EServer, page: Page, path: string): Promise<void> {
  const admin = server.admin;
  if (!admin) throw new Error('the e2e server has no admin');
  await server.signIn(page.context(), admin);
  await page.goto(`${server.url}${path}`);
}

test('an admin creates an invite link, and revokes it so that it stops working', async ({
  startServer,
  page,
  browser,
}) => {
  const own = await startServer();
  await openAdmin(own, page, '/admin/invites');
  await expect(page.getByRole('heading', { level: 1, name: 'Invites' })).toBeVisible();

  const note = uniqueName('admin-spec');
  await page.getByLabel('Note').fill(note);
  await page.getByRole('button', { name: 'Create link' }).click();

  // The link shows once, selected, with the focus.
  const field = page.getByLabel('Invite link', { exact: true });
  await expect(field).toBeFocused();
  const url = await field.inputValue();
  expect(url).toMatch(new RegExp(`^${own.url.replaceAll('.', '\\.')}/invite#.+`));
  await page.getByRole('button', { name: 'Done' }).click();
  await expect(field).toHaveCount(0);

  const row = page.getByRole('row', { name: new RegExp(note) });
  await expect(row).toContainText('Active');

  // Revoke asks first, then the invite leaves the list of working ones.
  await row.getByRole('button', { name: `Revoke the invite "${note}"` }).click();
  const dialog = page.getByRole('dialog', { name: 'Revoke this invite?' });
  await expect(dialog).toContainText('The link stops working right away.');
  await dialog.getByRole('button', { name: 'Revoke' }).click();
  await expect(dialog).toHaveCount(0);
  await expect(row).toHaveCount(0);
  await page.getByLabel(/^Show invites that no longer work/).check();
  await expect(row).toContainText('Revoked');

  // A friend who opens the link is told that it was revoked, and gets no form.
  const friend = await browser.newContext();
  try {
    const friendPage = await friend.newPage();
    await friendPage.goto(url);
    await expect(friendPage.getByRole('heading', { name: "This invite link doesn't work" })).toBeVisible();
    await expect(friendPage.getByText('This invite link was revoked. Ask for a new one.')).toBeVisible();
    await expect(friendPage.getByLabel('Username')).toHaveCount(0);
  } finally {
    await friend.close();
  }
});

test('an admin approves a pending sign-up in approval mode, and the friend can log in', async ({
  startServer,
  page,
  browser,
}) => {
  const own = await startServer({ overrides: { 'registration.mode': 'approval' } });

  // A friend asks for an account, in a browser of their own.
  const account: Account = { id: '', username: uniqueName('asker'), password: `${uniqueName('long-horse')}-staple` };
  const friend = await browser.newContext();
  await enableDebug(friend);
  try {
    const friendPage = await friend.newPage();
    await friendPage.goto(`${own.url}/signup`);
    await expect(friendPage.getByRole('heading', { name: 'Request an account' })).toBeVisible();
    await friendPage.getByLabel('Username').fill(account.username);
    await friendPage.getByLabel('Password', { exact: true }).fill(account.password);
    await friendPage.getByRole('button', { name: 'Send request' }).click();
    await expect(friendPage.getByRole('heading', { name: 'Request sent' })).toBeVisible();

    // The admin sees it waiting, beside Approvals and on its page, and approves it.
    await openAdmin(own, page, '/admin/approvals');
    await expect(page.getByRole('heading', { level: 1, name: 'Approvals' })).toBeVisible();
    await expect(page.getByRole('link', { name: 'Approvals, 1 waiting' })).toBeVisible();
    const row = page.getByRole('row', { name: new RegExp(`^${account.username}`) });
    await expect(row).toBeVisible();
    await row.getByRole('button', { name: `Approve ${account.username}` }).click();
    await expect(row).toHaveCount(0);
    await expect(page.getByText('No sign-ups are waiting.')).toBeVisible();

    // The Users page lists the account as active.
    await page.getByRole('navigation', { name: 'Admin' }).getByRole('link', { name: 'Users' }).click();
    await expect(page.getByRole('row', { name: new RegExp(`^${account.username}`) })).toContainText('Active');

    // And the friend logs in and lands in the lounge.
    await own.signIn(friend, account);
    await friendPage.goto(`${own.url}/`);
    await waitForRoom(friendPage, 'lounge');
  } finally {
    await friend.close();
  }
});

/** The page's list of checks, and one check in it by its name. */
function checks(page: Page): Locator {
  return page.getByRole('list', { name: 'Checks' });
}

test('the doctor page runs doctor and renders the server’s results', async ({ startServer, page }) => {
  // A doctor run has two minutes in the server (04 §13.1); an e2e server's takes about a second.
  test.setTimeout(180_000);
  const own = await startServer();
  await openAdmin(own, page, '/admin/doctor');
  await expect(page.getByRole('heading', { level: 1, name: 'Doctor' })).toBeVisible();

  // A new server has its first report 20 s after its start; until then the page says there is none. Running doctor
  // may meet the server's own run (429 doctor_busy): the page then offers the button again after the wait.
  const run = page.getByRole('button', { name: /^Run doctor/ });
  await expect(run).toBeVisible();
  const ran = (r: Response) => new URL(r.url()).pathname === '/api/v1/admin/doctor' && r.request().method() === 'POST';
  await expect(async () => {
    await expect(run).toBeEnabled({ timeout: 15_000 });
    const [response] = await Promise.all([page.waitForResponse(ran, { timeout: 130_000 }), run.click()]);
    expect(response.status()).toBe(200);
  }).toPass({ timeout: 160_000 });
  await expect(checks(page)).toBeVisible();

  const list = checks(page);
  // Every check of 04 §13.2, each with its name, its status and a sentence from the catalog.
  const items = list.getByRole('listitem');
  await expect(items).toHaveCount(17);
  const texts = await items.allInnerTexts();
  for (const text of texts) {
    expect(text).not.toMatch(/\{\{|doctor\.[a-z_]+\.[a-z_]+/);
  }

  // README F04: on a development server public_ip is not checked, which is no failure.
  const publicIp = list.getByRole('listitem', { name: 'Public IP' });
  await expect(publicIp).toContainText('Not checked');
  await expect(publicIp).toContainText(
    'Not checked: network.include_loopback is on and network.stun_servers is empty (a development setup, which ' +
      'needs no public address).',
  );
  await expect(list.getByRole('listitem', { name: 'Config' })).toContainText('OK');
  await expect(list.getByRole('listitem', { name: 'Certificate' })).toContainText(
    'Off: the proxy in front has the certificate.',
  );
  const ports = list.getByRole('listitem', { name: 'Ports' });
  await expect(ports).toContainText('This machine only');
  await expect(ports).toContainText('Listening on');

  // The calculator answers from the server: 04 §13.4's M1 exit row.
  const calculator = page.getByRole('region', { name: 'Bandwidth calculator' });
  await expect(calculator.getByRole('status')).toContainText('41.54 Mbps');
  await calculator.getByLabel('People').fill('10');
  await calculator.getByLabel('Sharing at once').fill('10');
  await calculator.getByRole('button', { name: 'Calculate' }).click();
  await expect(calculator.getByRole('status')).toContainText('10 people, 10 sharing, for 2 hours:');

  // The connection test is on the page too.
  await expect(page.getByRole('region', { name: 'Connection test' })).toBeVisible();
});

test('the dashboard shows the live server and asks for it every 2 seconds', async ({ startServer, page }) => {
  const own = await startServer();
  const polls: Request[] = [];
  page.on('request', (request) => {
    if (new URL(request.url()).pathname === '/api/v1/admin/dashboard') polls.push(request);
  });
  await openAdmin(own, page, '/admin');
  await expect(page.getByRole('heading', { level: 1, name: 'Dashboard' })).toBeVisible();
  await expect(page.getByRole('region', { name: 'Right now' })).toContainText('Sending now');
  await expect(page.getByRole('region', { name: 'Server' })).toContainText(own.url);
  // The setup was done over REST, not through the wizard, so the checklist leads there.
  await expect(page.getByRole('region', { name: 'Finish setting up' })).toBeVisible();

  const first = polls.length;
  await expect.poll(() => polls.length, { timeout: 10_000 }).toBeGreaterThanOrEqual(first + 2);
});
