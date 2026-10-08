// The names of the presets and of what is shared, as the ShareSheet and the share panel show them (05 §13.5).
import type { TFunction } from 'i18next';

import {
  PresetGame,
  PresetMovie,
  PresetText,
  ShareKindTab,
  ShareKindWindow,
  type Preset,
  type ShareKind,
} from '../protocol/types.gen';

/** A preset's name and its one line. Literal keys, so check:i18n sees each one. Unknown values read as Auto (01). */
export function presetText(preset: Preset, t: TFunction): { name: string; hint: string } {
  switch (preset) {
    case PresetGame:
      return { name: t('share.preset.game.name'), hint: t('share.preset.game.hint') };
    case PresetMovie:
      return { name: t('share.preset.movie.name'), hint: t('share.preset.movie.hint') };
    case PresetText:
      return { name: t('share.preset.text.name'), hint: t('share.preset.text.hint') };
    default:
      return { name: t('share.preset.auto.name'), hint: t('share.preset.auto.hint') };
  }
}

/** "Window", "Tab" or "Screen": what is shared, by kind, never a window title. Unknown values read as Screen (01). */
export function kindText(kind: ShareKind, t: TFunction): string {
  switch (kind) {
    case ShareKindWindow:
      return t('share.label.window');
    case ShareKindTab:
      return t('share.label.tab');
    default:
      return t('share.label.screen');
  }
}
