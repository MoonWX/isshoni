// What the native share UI does with a failure (docs/m2/03 §10.5, §6.4): the bridge's error codes, engine.<media code>
// with params.reason (04 §5.7's detail token), chosen by reason first and then by code. A reason row wins over its
// code's row; an unknown reason is shown with its code's text; an unknown engine code with engine.internal's.
//
//   reason or code                              text                                  what is offered
//   pick_expired (start; the controller's own)  none                                  the OS picker reopens
//   self_window (pick)                          "Pick something other than isshoni"   the OS picker reopens
//     (once per Share click; when it isn't reopened, shareFailureText says "That pick timed out…" for pick_expired,
//     and the note offers [Pick again])
//   driver_too_old                              update the graphics driver            /troubleshooting#desktop-gpu-driver
//   no_media_foundation                         the Media Feature Pack                #desktop-media-feature-pack
//   blocked_by_policy                           Smart App Control or a policy         #desktop-smart-app-control
//   missing, version, abi                       reinstall isshoni                     the server's /download page
//   device_removed (encoder_lost while live)    the graphics driver restarted         Share again (a new pick)
//   engine.permission_denied                    Windows didn't allow the capture      Windows settings, Try again
//   engine.encoder_lost                         the video encoder stopped             Settings → Sharing
//   engine.source_gone                          the window was closed                 —
//   engine.unsupported_os, engine.no_encoder    sharing needs …, no H.264 encoder     —
//   engine.dependency without a reason row      no H.264 encoder works                —
//   engine.internal, engine.invalid, .busy      something went wrong in the engine    Open logs folder
//   engine.cancelled                            never shown (Share.Pick() answers null)
//   publisher_offline                           can't reach the server                "See why" (#desktop-linking)
//   server_unreachable ending a live share      "Your share stopped: … 30 seconds"    Share again
//     (01's 30 s hold ran out: DesktopShare's endError, SHARE_HOLD_ENDED)
//   any other bridge code                       desktop.errors.<code>
//   a wire code from C2 (room_full, …)          M1's errors.<code>
//
// "Enable Cisco OpenH264…" waits for 04's opt-in (README WA51) and Settings → Sharing (WA84).
import type { TFunction } from 'i18next';

import { i18n } from '../../i18n';
import { errorMessage } from '../../lib/errorText';
import { SHARE_HOLD_ENDED } from '../../platform/desktop/DesktopShare';
import { BridgeError } from '../../platform/desktop/errors';

/** What the UI offers next to the text. */
export type ShareErrorAction =
  /**
   * Opens the OS picker again (pick_expired, self_window); for pick_expired without a word. Where it isn't reopened
   * by itself, the note offers [Pick again].
   */
  | 'pick-again'
  /** App.OpenOSSettings('graphics-capture') and [Try again] (permission_denied). */
  | 'capture-settings'
  /** The project site's troubleshooting page at `anchor` (on the server: serverOrigin + '/troubleshooting'). */
  | 'troubleshooting'
  /** The server's Download page (a damaged install). */
  | 'download'
  /** [Share again]: a new pick (device_removed, a share whose server was out of reach for 30 s). */
  | 'share-again'
  /** Settings → Sharing (/desktop/settings#sharing). */
  | 'sharing-settings'
  /** App.OpenLogsFolder(): the detail is in the log, never on screen (01 §5.2). */
  | 'logs';

export interface ShareErrorView {
  /** The message's key; null when nothing is said (pick_expired, cancelled). */
  readonly key: string | null;
  readonly action: ShareErrorAction | null;
  /** For 'troubleshooting': the anchor on /troubleshooting. */
  readonly anchor?: string;
  /** publisher_offline: also "Opens in your browser but not here? See why" (docs/m2/03 §9). */
  readonly seeWhy?: boolean;
}

const ENGINE = 'engine.';

/** The engine's media code of a bridge error code, or null for another code. */
function mediaCode(code: string): string | null {
  return code.startsWith(ENGINE) ? code.slice(ENGINE.length) : null;
}

/** Whether the desktop catalog has a message here. */
function has(key: string): boolean {
  return i18n.exists(key);
}

/** The view of a reason row; null when the reason has none. */
function byReason(reason: string): ShareErrorView | null {
  switch (reason) {
    case 'pick_expired':
      return { key: null, action: 'pick-again' };
    case 'self_window':
      return { key: 'desktop.errors.engine.reason.selfWindow', action: 'pick-again' };
    case 'driver_too_old':
      return {
        key: 'desktop.errors.engine.reason.driverTooOld',
        action: 'troubleshooting',
        anchor: 'desktop-gpu-driver',
      };
    case 'no_media_foundation':
      return {
        key: 'desktop.errors.engine.reason.noMediaFoundation',
        action: 'troubleshooting',
        anchor: 'desktop-media-feature-pack',
      };
    case 'blocked_by_policy':
      return {
        key: 'desktop.errors.engine.reason.blockedByPolicy',
        action: 'troubleshooting',
        anchor: 'desktop-smart-app-control',
      };
    case 'missing':
    case 'version':
    case 'abi':
      return { key: 'desktop.errors.engine.reason.reinstall', action: 'download' };
    case 'device_removed':
      return { key: 'desktop.errors.engine.reason.deviceRemoved', action: 'share-again' };
    default:
      return null;
  }
}

/** The view of an engine code's row (no reason row applied). */
function byMediaCode(code: string): ShareErrorView {
  switch (code) {
    case 'cancelled':
      return { key: null, action: null };
    case 'permission_denied':
      return { key: 'desktop.errors.engine.permission_denied', action: 'capture-settings' };
    case 'encoder_lost':
      return { key: 'desktop.errors.engine.encoder_lost', action: 'sharing-settings' };
    case 'dependency':
      return { key: 'desktop.errors.engine.no_encoder', action: null };
    case 'internal':
    case 'invalid':
    case 'busy':
      return { key: 'desktop.errors.engine.internal', action: 'logs' };
    default: {
      const key = `desktop.errors.engine.${code}`;
      return has(key) ? { key, action: null } : { key: 'desktop.errors.engine.internal', action: 'logs' };
    }
  }
}

/**
 * The UI's view of a native share's failure (a BridgeError: a pick, a start, a share that failed while live); null
 * for anything else (M1's errors, which errorMessage describes).
 */
export function shareErrorView(err: unknown): ShareErrorView | null {
  if (!(err instanceof BridgeError)) return null;
  const reason = typeof err.params['reason'] === 'string' ? err.params['reason'] : '';
  const media = mediaCode(err.code);
  if (err.code === 'pick_expired') return { key: null, action: 'pick-again' };
  if (media !== null) return byReason(reason) ?? byMediaCode(media);
  if (err.code === 'publisher_offline') return { key: 'desktop.errors.publisher_offline', action: null, seeWhy: true };
  if (err.code === 'server_unreachable' && err.params['ended'] === SHARE_HOLD_ENDED.ended) {
    return { key: 'desktop.share.errors.serverUnreachable', action: 'share-again' };
  }
  const key = `desktop.errors.${err.code}`;
  if (has(key)) return { key, action: null };
  // A wire code C2 passed through (room_full, share_limit, …): M1's text.
  return { key: has(`errors.${err.code}`) ? `errors.${err.code}` : 'desktop.errors.internal', action: null };
}

/** Whether a failure only means "pick again": the OS picker reopens (pick_expired, self_window). */
export function isPickAgain(err: unknown): boolean {
  return shareErrorView(err)?.action === 'pick-again';
}

/**
 * The text of a native share's failure: the view's message, else M1's (errorMessage); '' when nothing is to be said
 * (pick_expired, cancelled).
 */
export function shareErrorText(err: unknown, t: TFunction = i18n.t): string {
  const view = shareErrorView(err);
  if (view === null) return errorMessage(err, t);
  return view.key === null ? '' : t(view.key);
}

/**
 * The text of a failure that stays on screen (the panel, a toast) instead of reopening the picker: shareErrorText,
 * and for a pick Go no longer holds, which says nothing while the picker reopens, "That pick timed out. Pick what to
 * share again." ('' only for cancelled).
 */
export function shareFailureText(err: unknown, t: TFunction = i18n.t): string {
  const text = shareErrorText(err, t);
  if (text !== '' || shareErrorView(err)?.action !== 'pick-again') return text;
  return t('desktop.errors.pick_expired');
}
