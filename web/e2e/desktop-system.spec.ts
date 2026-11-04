// desktop-system.spec (M2 03 §11.6, §22.4 A, README WA68): what the system does to a native share, with the app's
// real Go parts in internal/desktop/e2ehost and the OS events injected through POST /__e2e/system:
// - a screen lock pauses the live share through the share controller (01's share.update {paused}), and keep-awake is
//   released; an unlock right after it (well within 03 §11.6's 5 min) resumes the share and takes keep-awake again;
// - a suspend stops the share with the end reason system and releases keep-awake; the resume shows the toast
//   "Sharing stopped because your PC went to sleep".
//
// The long-lock cases (an unlock after the first bound stays paused with "Resume sharing?" in the panel, the pill,
// the tray and a toast; a lock pause stops at the second bound) need the harness's -lock-resume/-lock-stop wired to
// the app and the panel's bar: README WA80 extends this spec with them (03 §22.4 A's row).
import { expect, test, type DesktopApp, type DesktopState, type E2EServer } from './fixtures.ts';
import { waitForRoom } from './stats.ts';

/** C2 connects, publishes and goes live. */
const LIVE_TIMEOUT_MS = 30_000;
const STATE_TIMEOUT_MS = 10_000;

test.describe.configure({ timeout: 120_000 });

/** The bridge's ShareView, as far as this spec reads it. */
interface ShareView {
  state: string;
  endReason: string | null;
}

/** A recorded toast (app.Notification as the harness's JSON has it). */
interface Toast {
  Kind: string;
  Body: string;
  Nonce: string;
  Actions: { ID: string; Title: string }[] | null;
}

/** A desktop app linked to the worker's server as a new member, with its page in `roomId`. */
async function linkedDesktop(
  startDesktop: (o?: { server?: string }) => Promise<DesktopApp>,
  server: E2EServer,
  roomId: string,
): Promise<DesktopApp> {
  const account = await server.createUser();
  const desktop = await startDesktop({ server: server.url });
  const st = await desktop.call<{ status: string }>('Auth', 'LinkWithPassword', account.username, account.password);
  expect(st.status).toBe('linked');
  await desktop.page.goto(`${desktop.origin}/r/${roomId}`);
  await waitForRoom(desktop.page, roomId);
  return desktop;
}

/** Picks the fake engine's window and starts sharing it in `roomId`, then waits until the share is live. */
async function shareLive(desktop: DesktopApp, roomId: string): Promise<void> {
  const picked = await desktop.call<{ pickId: string } | null>('Share', 'Pick');
  expect(picked, 'the fake picker picks').not.toBeNull();
  await desktop.call('Share', 'Start', { pickId: picked?.pickId, roomId, preset: 'auto', withAudio: false });
  await expectShare(desktop, 'live', LIVE_TIMEOUT_MS);
}

/** Waits until Share.Current() is in `state`, and returns it. */
async function expectShare(desktop: DesktopApp, state: string, timeout = STATE_TIMEOUT_MS): Promise<ShareView> {
  const current = (): Promise<ShareView | null> => desktop.call<ShareView | null>('Share', 'Current');
  await expect
    .poll(async () => (await current())?.state ?? null, { timeout, message: `the share is ${state}` })
    .toBe(state);
  const view = await current();
  expect(view?.state, `the share is still ${state}`).toBe(state);
  if (view === null) throw new Error('no share');
  return view;
}

/** Waits until the harness's keep-awake is `on`. */
async function expectKeepAwake(desktop: DesktopApp, on: boolean): Promise<void> {
  await expect
    .poll(async () => (await desktop.state()).keepAwake, {
      timeout: STATE_TIMEOUT_MS,
      message: `keep-awake is ${on ? 'on' : 'off'}`,
    })
    .toBe(on);
}

function toastsOf(st: DesktopState, kind: string): Toast[] {
  return (st.toasts as Toast[]).filter((t) => t.Kind === kind);
}

test('a lock pauses the share and an unlock resumes it; a suspend stops it with a toast', async ({
  startDesktop,
  server,
}) => {
  const room = 'lounge';
  const desktop = await linkedDesktop(startDesktop, server, room);
  await shareLive(desktop, room);
  await expectKeepAwake(desktop, true);

  // The screen locks: the controller pauses the share (01's pause, share.update {paused: true}) and the PC may sleep.
  await desktop.system('lock');
  await expectShare(desktop, 'paused');
  await expectKeepAwake(desktop, false);

  // The unlock comes at once, within the 5 min: the share resumes, and keep-awake is back.
  await desktop.system('unlock');
  await expectShare(desktop, 'live');
  await expectKeepAwake(desktop, true);
  expect(toastsOf(await desktop.state(), 'lock-paused'), 'no "Resume sharing?" after a short lock').toEqual([]);

  // The PC goes to sleep: the share stops with the end reason system, keep-awake goes off.
  await desktop.system('suspend');
  const ended = await expectShare(desktop, 'ended');
  expect(ended.endReason).toBe('system');
  await expectKeepAwake(desktop, false);
  expect(toastsOf(await desktop.state(), 'stopped-sleep'), 'no toast while the PC sleeps').toEqual([]);

  // It wakes: the toast says what happened, with Open.
  await desktop.system('resume');
  await expect
    .poll(async () => toastsOf(await desktop.state(), 'stopped-sleep').length, { timeout: STATE_TIMEOUT_MS })
    .toBe(1);
  const [toast] = toastsOf(await desktop.state(), 'stopped-sleep');
  expect(toast?.Body).toBe('Sharing stopped because your PC went to sleep');
  expect(toast?.Actions?.map((a) => a.ID)).toEqual(['open']);
  expect(toast?.Nonce).toMatch(/^[0-9a-f]{32}$/);

  // The tray, when the harness has one (README WA68's recording SetTray), is idle again.
  const { tray } = await desktop.state();
  if (tray === null) {
    test.info().annotations.push({ type: 'tray', description: 'the harness records no tray yet' });
  } else {
    await expect.poll(async () => JSON.stringify((await desktop.state()).tray)).toContain('tray-idle');
  }
});
