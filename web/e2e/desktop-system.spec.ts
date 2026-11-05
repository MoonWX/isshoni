// desktop-system.spec (M2 03 §11.6, §22.4 A, README WA68): what the system does to a native share, with the app's
// real Go parts in internal/desktop/e2ehost and the OS events injected through POST /__e2e/system:
// - the page beats App.Heartbeat at once and every 5 s, which arms W17's watchdog (03 §3.2, §20);
// - a screen lock pauses the live share through the share controller (01's share.update {paused}), and keep-awake is
//   released; an unlock right after it (well within 03 §11.6's 5 min) resumes the share and takes keep-awake again;
// - a suspend stops the share with the end reason system and releases keep-awake; the resume shows the toast
//   "Sharing stopped because your PC went to sleep";
// - an unlock after the first bound (lockResumeMs) leaves the share paused and asks "Resume sharing?" with the tray's
//   Paused menu and a toast with Resume and Stop; a resume through the controller (the panel's) ends the lock pause,
//   so keep-awake is back and the second bound no longer stops the share;
// - a lock pause that reaches the second bound (lockStopMs) stops the share with the end reason system and the toast
//   "Sharing stopped while your PC was locked";
// - the end of the session: WM_QUERYENDSESSION stops the share and is acknowledged while the app keeps running (the
//   end may be cancelled); WM_ENDSESSION (end-session-confirmed) quits the app, acknowledged once it has.
//
// The panel's "Resume sharing?" bar and the pill's line are README WA80's: it adds them to the long-lock case.
import {
  expect,
  test,
  type DesktopApp,
  type DesktopState,
  type E2EServer,
  type StartDesktopOptions,
} from './fixtures.ts';
import { waitForRoom } from './stats.ts';

/** C2 connects, publishes and goes live. */
const LIVE_TIMEOUT_MS = 30_000;
const STATE_TIMEOUT_MS = 10_000;
/** The page's heartbeat interval (DesktopPlatform's HeartbeatEveryMs, app.HeartbeatEvery). */
const HEARTBEAT_MS = 5_000;
/** The long-lock cases' bounds: 03 §22.4 A's -lock-resume=2s -lock-stop=6s (the unlock comes after 3 s). */
const LOCK_RESUME_MS = 2_000;
const LOCK_STOP_MS = 6_000;

test.describe.configure({ timeout: 120_000 });

/** The bridge's ShareView, as far as this spec reads it. */
interface ShareView {
  state: string;
  endReason: string | null;
}

/** A recorded toast (app.Notification as the harness's JSON has it). */
interface Toast {
  ID: string;
  Kind: string;
  Body: string;
  Nonce: string;
  Actions: { ID: string; Title: string }[] | null;
}

/** A desktop app linked to the worker's server as a new member, with its page in `roomId`. */
async function linkedDesktop(
  startDesktop: (o?: StartDesktopOptions) => Promise<DesktopApp>,
  server: E2EServer,
  roomId: string,
  options: Omit<StartDesktopOptions, 'server'> = {},
): Promise<DesktopApp> {
  const account = await server.createUser();
  const desktop = await startDesktop({ ...options, server: server.url });
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

/** Waits until exactly one toast of `kind` was shown, and returns it. */
async function expectToast(desktop: DesktopApp, kind: string): Promise<Toast> {
  await expect
    .poll(async () => toastsOf(await desktop.state(), kind).length, {
      timeout: STATE_TIMEOUT_MS,
      message: `one ${kind} toast`,
    })
    .toBe(1);
  const [toast] = toastsOf(await desktop.state(), kind);
  if (toast === undefined) throw new Error(`no ${kind} toast`);
  expect(toast.Nonce).toMatch(/^[0-9a-f]{32}$/);
  return toast;
}

/** The tray's view as text, or null when the harness records no tray. */
async function trayText(desktop: DesktopApp): Promise<string | null> {
  const { tray } = await desktop.state();
  return tray === null ? null : JSON.stringify(tray);
}

test('the page sends App.Heartbeat at once and every 5 s (W17)', async ({ startDesktop, server }) => {
  const desktop = await linkedDesktop(startDesktop, server, 'lounge');
  await expect
    .poll(async () => (await desktop.state()).heartbeats, { timeout: STATE_TIMEOUT_MS, message: 'the first beat' })
    .toBeGreaterThanOrEqual(1);
  const first = (await desktop.state()).heartbeats;
  await expect
    .poll(async () => (await desktop.state()).heartbeats, {
      timeout: HEARTBEAT_MS + STATE_TIMEOUT_MS,
      message: 'the next beat',
    })
    .toBeGreaterThan(first);
  // Beating, the visible page is never reloaded by the watchdog.
  expect((await desktop.state()).reloads).toBe(0);
});

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

  // The tray (the harness records SetTray) is idle again.
  await expect.poll(() => trayText(desktop), { timeout: STATE_TIMEOUT_MS }).toContain('tray-idle');
});

test('an unlock after the first bound asks "Resume sharing?"; a resume ends the lock pause', async ({
  startDesktop,
  server,
}) => {
  const room = 'lounge';
  const desktop = await linkedDesktop(startDesktop, server, room, {
    lockResumeMs: LOCK_RESUME_MS,
    lockStopMs: LOCK_STOP_MS,
  });
  await shareLive(desktop, room);
  await expectKeepAwake(desktop, true);

  await desktop.system('lock');
  const lockedAt = Date.now();
  await expectShare(desktop, 'paused');
  await expectKeepAwake(desktop, false);

  // The unlock comes after the first bound (03 §22.4 A: after 3 s): the share stays paused, and the app asks.
  await desktop.page.waitForTimeout(Math.max(0, lockedAt + LOCK_RESUME_MS + 1_000 - Date.now()));
  await desktop.system('unlock');
  const toast = await expectToast(desktop, 'lock-paused');
  expect(toast.Body).toBe('Sharing is still paused since you locked your PC. Resume sharing?');
  expect(toast.Actions?.map((a) => a.ID)).toEqual(['resume', 'stop']);
  expect((await desktop.call<ShareView | null>('Share', 'Current'))?.state).toBe('paused');
  // Keep-awake stays off while the question waits.
  expect((await desktop.state()).keepAwake).toBe(false);
  await expect.poll(() => trayText(desktop), { timeout: STATE_TIMEOUT_MS }).toContain('tray-paused');

  // Resume from outside the app's own prompt (the panel calls the controller): the lock pause ends.
  await desktop.call('Share', 'SetPaused', false);
  await expectShare(desktop, 'live');
  await expectKeepAwake(desktop, true);

  // The second bound passes: the resumed share isn't stopped, and no "stopped while locked" toast comes.
  await desktop.page.waitForTimeout(Math.max(0, lockedAt + LOCK_STOP_MS + 1_000 - Date.now()));
  expect((await desktop.call<ShareView | null>('Share', 'Current'))?.state).toBe('live');
  expect(toastsOf(await desktop.state(), 'stopped-locked')).toEqual([]);
  expect((await desktop.state()).keepAwake).toBe(true);

  // The question was answered, so its toast has left Action Center (03 §15) by now, once the app calls the OS
  // hooks' removal (winos.Hooks.RemoveToasts, which the harness records). The app doesn't call it yet: the slice that
  // owns internal/desktop/app wires it (toastBook.dropKind's IDs, on the serialized toast path) and turns this into
  // a hard expect.poll(...).toEqual([toast.ID]).
  const { removedToasts } = await desktop.state();
  if (removedToasts.length === 0) {
    test.info().annotations.push({ type: 'action-center', description: 'the app removes no toasts yet' });
  } else {
    expect(removedToasts).toEqual([toast.ID]);
  }
});

test('a lock pause that reaches the second bound stops the share with a toast', async ({ startDesktop, server }) => {
  const room = 'lounge';
  const desktop = await linkedDesktop(startDesktop, server, room, {
    lockResumeMs: LOCK_RESUME_MS,
    lockStopMs: LOCK_STOP_MS,
  });
  await shareLive(desktop, room);

  await desktop.system('lock');
  await expectShare(desktop, 'paused');
  // Still locked at the second bound: the share stops with the end reason system.
  const ended = await expectShare(desktop, 'ended', LOCK_STOP_MS + STATE_TIMEOUT_MS);
  expect(ended.endReason).toBe('system');
  const toast = await expectToast(desktop, 'stopped-locked');
  expect(toast.Body).toBe('Sharing stopped while your PC was locked');
  expect(toast.Actions?.map((a) => a.ID)).toEqual(['open']);
  await expectKeepAwake(desktop, false);

  // The unlock afterwards asks nothing: the share is gone.
  await desktop.system('unlock');
  await desktop.page.waitForTimeout(500);
  expect(toastsOf(await desktop.state(), 'lock-paused')).toEqual([]);
  expect((await desktop.state()).keepAwake).toBe(false);
});

test('the end of the session stops the share, and the confirmed end quits the app', async ({
  startDesktop,
  server,
}) => {
  const room = 'lounge';
  const desktop = await linkedDesktop(startDesktop, server, room);
  await shareLive(desktop, room);

  // WM_QUERYENDSESSION: the share stops (share.stop, end reason system) and Windows is told it may go on.
  await desktop.system('end-session');
  const ended = await expectShare(desktop, 'ended');
  expect(ended.endReason).toBe('system');
  await expect.poll(async () => (await desktop.state()).endSessionAcks, { timeout: STATE_TIMEOUT_MS }).toBe(1);
  // The end may still be cancelled: the app keeps running.
  expect((await desktop.state()).quit).toBe(false);
  await expectKeepAwake(desktop, false);

  // WM_ENDSESSION with TRUE: the app quits, and acknowledges only once it has.
  await desktop.system('end-session-confirmed');
  await expect.poll(async () => (await desktop.state()).endSessionAcks, { timeout: STATE_TIMEOUT_MS }).toBe(2);
  expect((await desktop.state()).quit).toBe(true);
});
