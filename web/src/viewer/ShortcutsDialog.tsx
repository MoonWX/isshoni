// The shortcuts dialog (05 §12.7): what the "?" key and the stage's keyboard button open. It lists the keys of
// keyboard.ts with what they do. A native <dialog> (ui/Dialog), so Esc closes it and the focus goes back to where
// it was.
import type { TFunction } from 'i18next';
import { useTranslation } from 'react-i18next';

import { Dialog } from '../ui/Dialog';
import { SHORTCUT_ROWS, type ShortcutRow } from './keyboard';
import styles from './ShortcutsDialog.module.css';

/** The keys of a row, as the catalog writes them: key names are words too ("Esc", "Arrow keys"). */
function keysText(t: TFunction, row: ShortcutRow): string {
  switch (row) {
    case 'move':
      return t('viewer.shortcuts.keys.move');
    case 'pick':
      return t('viewer.shortcuts.keys.pick');
    case 'nth':
      return t('viewer.shortcuts.keys.nth');
    case 'fullscreen':
      return t('viewer.shortcuts.keys.fullscreen');
    case 'mute':
      return t('viewer.shortcuts.keys.mute');
    case 'listen':
      return t('viewer.shortcuts.keys.listen');
    case 'escape':
      return t('viewer.shortcuts.keys.escape');
    case 'help':
      return t('viewer.shortcuts.keys.help');
    case 'debug':
      return t('viewer.shortcuts.keys.debug');
  }
}

function actionText(t: TFunction, row: ShortcutRow): string {
  switch (row) {
    case 'move':
      return t('viewer.shortcuts.actions.move');
    case 'pick':
      return t('viewer.shortcuts.actions.pick');
    case 'nth':
      return t('viewer.shortcuts.actions.nth');
    case 'fullscreen':
      return t('viewer.shortcuts.actions.fullscreen');
    case 'mute':
      return t('viewer.shortcuts.actions.mute');
    case 'listen':
      return t('viewer.shortcuts.actions.listen');
    case 'escape':
      return t('viewer.shortcuts.actions.escape');
    case 'help':
      return t('viewer.shortcuts.actions.help');
    case 'debug':
      return t('viewer.shortcuts.actions.debug');
  }
}

export interface ShortcutsDialogProps {
  open: boolean;
  onClose: () => void;
  /** Rows to leave out: a key that does nothing here is not listed. */
  without?: readonly ShortcutRow[];
}

export function ShortcutsDialog({ open, onClose, without = [] }: ShortcutsDialogProps) {
  const { t } = useTranslation();
  return (
    <Dialog open={open} onClose={onClose} title={t('viewer.shortcuts.title')} size="sm">
      <dl className={styles.list}>
        {SHORTCUT_ROWS.filter((row) => !without.includes(row)).map((row) => (
          <div key={row} className={styles.row}>
            <dt>
              <kbd className={styles.keys}>{keysText(t, row)}</kbd>
            </dt>
            <dd>{actionText(t, row)}</dd>
          </div>
        ))}
      </dl>
    </Dialog>
  );
}
