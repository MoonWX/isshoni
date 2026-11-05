// The texts of 05's audio rows, banners and prompts (05 §14.7–§14.8, docs/m2/03 §10.3, §18.1): every one is a
// `desktop.audio.*` key of the lazy desktop namespace, chosen by the code 05 sends. An app's name is shown, never
// logged or sent anywhere (05 §16).
import type { TFunction } from 'i18next';

import type { AppRow, Banner, Prompt } from '../../platform/desktop/bridge';

/** desktop.audio.reason.<key> by AppRow.reason (05 §14.5); '' (heard) has none. */
const REASON_KEYS: Record<AppRow['reason'], string | null> = {
  '': null,
  builtin: 'desktop.audio.reason.builtin',
  user: 'desktop.audio.reason.user',
  mic: 'desktop.audio.reason.mic',
  viewer: 'desktop.audio.reason.viewer',
  isshoni: 'desktop.audio.reason.isshoni',
  launcher: 'desktop.audio.reason.launcher',
  tree: 'desktop.audio.reason.tree',
  'pid-reuse': 'desktop.audio.reason.pidReuse',
  limit: 'desktop.audio.reason.limit',
  'capture-failed': 'desktop.audio.reason.captureFailed',
};

/** Why a row is kept out or not captured; '' for a heard row or a reason this build doesn't know. */
export function reasonText(row: AppRow, t: TFunction): string {
  const key = REASON_KEYS[row.reason] ?? null;
  return key === null ? '' : t(key, { detail: row.detail ?? '' });
}

/**
 * A row's warning (05 §14.5's `warning`), or, for a built-in voice app that is the shared app (locked: always kept
 * out), desktop.audio.warning.sharedVoiceApp (docs/m2/03 §10.3). '' for none.
 */
export function warningText(row: AppRow, t: TFunction): string {
  switch (row.warning) {
    case 'shared-uses-mic':
      return t('desktop.audio.warning.sharedUsesMic', { app: row.name });
    case 'shared-kept-out':
      return t('desktop.audio.warning.sharedKeptOut', { app: row.name });
    case 'included-uses-mic':
      return t('desktop.audio.warning.includedUsesMic', { app: row.name });
    default:
      return row.shared && row.control === 'locked' && row.reason === 'builtin'
        ? t('desktop.audio.warning.sharedVoiceApp', { app: row.name })
        : '';
  }
}

/** A banner's text (05 §14.8): sound-off by its cause, capture-trouble with its app. '' for an unknown code. */
export function bannerText(b: Banner, t: TFunction): string {
  switch (b.code) {
    case 'exclusion-unavailable':
      return t('desktop.audio.banner.exclusionUnavailable');
    case 'windows-too-old':
      return t('desktop.audio.banner.windowsTooOld');
    case 'no-output-device':
      return t('desktop.audio.banner.noOutputDevice');
    case 'reconnecting-sound':
      return t('desktop.audio.banner.reconnectingSound');
    case 'sound-off':
      return b.params?.['cause'] === 'paused'
        ? t('desktop.audio.banner.soundPaused')
        : t('desktop.audio.banner.soundOff');
    case 'capture-trouble':
      return t('desktop.audio.banner.captureTrouble', { app: String(b.params?.['app'] ?? '') });
    case 'viewer-muted':
      return t('desktop.audio.banner.viewerMuted');
    case 'viewer-unknown-browser':
      return t('desktop.audio.banner.viewerUnknownBrowser');
    default:
      return '';
  }
}

/** A mic prompt's question and its two buttons, by kind (docs/m2/03 §10.3, 05 §14.8). */
export function promptTexts(p: Prompt, t: TFunction): { question: string; keepOut: string; include: string } {
  const browser = p.kind === 'browser-mic';
  return {
    question: browser ? t('desktop.audio.prompt.browserMic') : t('desktop.audio.prompt.mic', { app: p.name }),
    keepOut: browser ? t('desktop.audio.prompt.keepBrowserOut') : t('desktop.audio.prompt.keepOut'),
    include: t('desktop.audio.prompt.include'),
  };
}
