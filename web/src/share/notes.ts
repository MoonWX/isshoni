// The notes of 05 §13.3: what to tell the sharer when a pick came without sound. Shown from the moment the share
// starts: in the share panel's bar while the share lasts, or as ShareButton's toast on a page without a panel.
import type { TFunction } from 'i18next';

import { ShareKindTab, ShareKindWindow } from '../protocol/types.gen';
import type { PickedInfo } from './shareStore';

/** The note for a pick without an audio track, by what was picked; null when the pick has sound. */
export function noAudioNote(picked: PickedInfo, t: TFunction): string | null {
  if (picked.warning !== 'no-audio') return null;
  switch (picked.kind) {
    case ShareKindWindow:
      return t('share.note.windowNoAudio');
    case ShareKindTab:
      return t('share.note.tabNoAudio');
    default:
      return t('share.note.screenNoAudio');
  }
}
