import type { ComponentPropsWithoutRef } from 'react';

import { cx } from './cx';
import styles from './VisuallyHidden.module.css';

/** Text for screen readers only: hidden visually, kept in the accessibility tree. */
export function VisuallyHidden({ className, ...props }: ComponentPropsWithoutRef<'span'>) {
  return <span {...props} className={cx(styles.root, className)} />;
}
