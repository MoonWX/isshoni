import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';

import { cx } from './cx';
import styles from './Spinner.module.css';
import { VisuallyHidden } from './VisuallyHidden';

export interface SpinnerProps {
  /** Diameter: sm 16 px (inside buttons), md 24 px, lg 40 px. */
  size?: 'sm' | 'md' | 'lg';
  /** What is loading, for screen readers; default "Loading". Pass null inside a control that already says it. */
  label?: string | null;
}

/**
 * A busy indicator. Under prefers-reduced-motion it stops turning and stays as a static ring (05 §16.6).
 */
export function Spinner({ size = 'md', label }: SpinnerProps) {
  const { t } = useTranslation();
  const text = label === undefined ? t('a11y.loading') : label;
  return (
    <span className={cx(styles.root, styles[size])} role={text === null ? undefined : 'status'}>
      <span className={styles.ring} aria-hidden="true" />
      {text !== null && <VisuallyHidden>{text}</VisuallyHidden>}
    </span>
  );
}

/**
 * A page-sized loading state, shown only after a short delay (300 ms) so fast loads don't flash.
 */
export function PageSpinner({ delayMs = 300 }: { delayMs?: number }) {
  const [visible, setVisible] = useState(delayMs <= 0);
  useEffect(() => {
    if (delayMs <= 0) return undefined;
    const id = setTimeout(() => {
      setVisible(true);
    }, delayMs);
    return () => {
      clearTimeout(id);
    };
  }, [delayMs]);
  return <div className={styles.page}>{visible && <Spinner size="lg" />}</div>;
}
