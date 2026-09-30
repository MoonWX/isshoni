import { cx } from '../ui/cx';
import styles from './AppMark.module.css';

/**
 * The isshoni mark: two screens side by side, one in front of the other ("together"). Decorative; the app name
 * next to it (or the page title) carries the meaning.
 */
export function AppMark({ size = 32, className }: { size?: number; className?: string }) {
  return (
    <svg
      className={cx(styles.mark, className)}
      width={size}
      height={size}
      viewBox="0 0 48 48"
      aria-hidden="true"
      focusable="false"
    >
      <rect className={styles.back} x="3" y="9" width="28" height="20" rx="4" />
      <rect className={styles.front} x="17" y="19" width="28" height="20" rx="4" />
      <path className={styles.stand} d="M27 44h8" />
    </svg>
  );
}
