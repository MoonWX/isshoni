import { cx } from './cx';
import { Dialog, type DialogProps } from './Dialog';
import styles from './Sheet.module.css';

export interface SheetProps extends Omit<DialogProps, 'size' | 'className'> {
  /**
   * bottom: slides up from the bottom edge (the ShareSheet, the iOS Home Screen sheet); end: a side drawer at the
   * inline end (the people panel). On phones both take the bottom edge.
   */
  side?: 'bottom' | 'end';
}

/** A modal sheet: a Dialog pinned to an edge of the screen. */
export function Sheet({ side = 'bottom', ...props }: SheetProps) {
  return <Dialog {...props} className={cx(styles.sheet, styles[side])} />;
}
