// tcp-only.spec (04 §17 "End-to-end", 05 §19.3; README S90): a network that blocks UDP. A server without UDP media
// (listen.ice_udp = "") stands in for it: its offers and answers carry no UDP candidate, so the browsers can only
// reach it over ICE-TCP. In off mode there is no 443 multiplexer (04 §8.5), so that is listen.ice_tcp, TCP 7882 in
// production. A friend still watches: the stage decodes the share's video, the friend's sub PeerConnection's
// selected candidate pair is TCP, and the server saw both PeerConnections, the sharer's pub and the friend's sub,
// connect through listen.ice_tcp.
//
// What is shared: in a headful Chrome (CI, under Xvfb) the tone tab, which Chrome's tab picker selects by its title;
// a headless Chrome shares the fake display of 05 §19.3. Everything after the pick is the same code.
import fsp from 'node:fs/promises';

import type { BrowserContext, Page } from '@playwright/test';

import { expect, fakeDisplay, openToneTab, test, type E2EServer } from './fixtures.ts';
import { pcOf, readState, videoOf, waitForRoom, waitForState, waitForStats } from './stats.ts';

/**
 * A share that started may take this long to play for a friend: a picker, two PeerConnections, and ICE-TCP, which
 * Chrome tries after its UDP candidates have nowhere to go.
 */
const MEDIA_TIMEOUT_MS = 30_000;

/** Gives `page` something to share, before it loads the app: the tone tab when headful, the fake display when not. */
async function prepareSource(context: BrowserContext, page: Page, server: E2EServer, headless: boolean): Promise<void> {
  if (headless) {
    await fakeDisplay(page, 'browser+audio');
    return;
  }
  const tone = await openToneTab(context, server);
  await tone.waitForFunction(() => (window as unknown as { __tone?: { audio: string } }).__tone?.audio === 'running');
}

/** Share in the header, then Share in the sheet; the page comes to the front first (05 §19.3). */
async function clickShare(page: Page): Promise<void> {
  await page.bringToFront();
  await page.getByRole('banner').getByRole('button', { name: 'Share', exact: true }).click();
  await page
    .getByRole('dialog', { name: 'Share your screen' })
    .getByRole('button', { name: 'Share', exact: true })
    .click();
}

test('with UDP blocked, a friend watches a share over ICE-TCP', async ({
  startServer,
  context,
  page,
  asUser,
  headless,
}) => {
  test.setTimeout(120_000);
  const server = await startServer({ overrides: { 'listen.ice_udp': '' } });
  const admin = server.admin;
  if (!admin) throw new Error('the server has no admin');
  await server.signIn(context, admin);
  await prepareSource(context, page, server, headless);
  const errors: string[] = [];
  page.on('pageerror', (error) => errors.push(String(error.stack ?? error)));
  await page.goto(`${server.url}/`);
  await waitForRoom(page, 'lounge');

  const friend = await asUser(server);
  friend.page.on('pageerror', (error) => errors.push(String(error.stack ?? error)));
  await friend.page.goto(`${server.url}/`);
  await waitForRoom(friend.page, 'lounge');

  await clickShare(page);
  const own = await waitForState(page, (s) => s.viewer.shares.find((share) => share.local && share.status === 'live'), {
    timeout: MEDIA_TIMEOUT_MS,
    message: "the sharer's share is live",
  });

  // The friend's stage plays it, over TCP.
  const first = await waitForStats(
    friend.page,
    (s) => {
      const sub = pcOf(s, 'sub');
      return (videoOf(s, own.id)?.framesDecoded ?? 0) > 0 && sub?.state === 'connected' && sub.transport === 'tcp' && s;
    },
    { timeout: MEDIA_TIMEOUT_MS, message: `decoded video of ${own.id} on a sub PC connected over TCP` },
  );
  // The browser's side of an ICE-TCP pair is an active candidate, which Chrome reports as host or prflx.
  expect(pcOf(first, 'sub')).toMatchObject({ state: 'connected', transport: 'tcp' });
  // And it goes on playing.
  await waitForStats(
    friend.page,
    (s) => (videoOf(s, own.id)?.framesDecoded ?? 0) > (videoOf(first, own.id)?.framesDecoded ?? 0) + 10 && s,
    { timeout: MEDIA_TIMEOUT_MS, message: 'more decoded frames than in the first sample' },
  );
  expect((await readState(friend.page)).viewer).toMatchObject({ focusedShareId: own.id, media: 'connected' });

  // The server's side, where both PCs show in one place (the friend's page has its sub PC, the sharer's page its pub
  // PC, 05 §10.7):
  // the SFU logs each PC's connected state with the listener its selected pair came through (02 §13). Both are
  // listen.ice_tcp's, and nothing connected over UDP.
  await expect
    .poll(async () => connectedVia(server), { message: 'the PCs that connected, with their transport (server log)' })
    .toEqual(expect.arrayContaining(['pub tcp7882', 'sub tcp7882']));
  expect((await connectedVia(server)).filter((pc) => !pc.endsWith(' tcp7882'))).toEqual([]);

  expect(errors).toEqual([]);
});

/** The server log's "PeerConnection state" lines for a connected PC, as "<pc> <transport>" ("pub tcp7882"). */
async function connectedVia(server: E2EServer): Promise<string[]> {
  const log = await fsp.readFile(server.logPath, 'utf8');
  const out: string[] = [];
  for (const line of log.split('\n')) {
    if (!line.includes('"PeerConnection state"')) continue;
    const rec = JSON.parse(line) as { msg?: string; pc?: string; state?: string; transport?: string };
    if (rec.msg === 'PeerConnection state' && rec.state === 'connected') {
      out.push(`${rec.pc ?? '?'} ${rec.transport ?? '?'}`);
    }
  }
  return out;
}
