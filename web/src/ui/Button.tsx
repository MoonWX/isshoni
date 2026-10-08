import type { ComponentPropsWithRef, ReactNode } from 'react';

import styles from './Button.module.css';
import { cx } from './cx';
import { Spinner } from './Spinner';

export type ButtonVariant = 'primary' | 'secondary' | 'ghost' | 'danger';
export type ButtonSize = 'md' | 'sm';

export interface ButtonStyleProps {
  /** primary: the one main action of a view; secondary: others; ghost: toolbars; danger: destructive actions. */
  variant?: ButtonVariant;
  /** md is touch-sized (44 px); sm is for dense lists on desktop. */
  size?: ButtonSize;
  /** Stretch to the container's width (forms on phones). */
  block?: boolean;
}

export interface ButtonProps extends ComponentPropsWithRef<'button'>, ButtonStyleProps {
  /** Shows a spinner and blocks clicks; the label stays for layout and screen readers (aria-busy). */
  loading?: boolean;
  /** A leading icon (lucide-react), decorative. */
  icon?: ReactNode;
}

/** The class names of a button, for links that look like one (<Link className={buttonClass(…)}>). */
export function buttonClass({ variant = 'secondary', size = 'md', block = false }: ButtonStyleProps = {}): string {
  return cx(styles.button, styles[variant], styles[size], block && styles.block);
}

export function Button({
  variant,
  size,
  block,
  loading = false,
  icon,
  className,
  disabled,
  children,
  type = 'button',
  onClick,
  ...props
}: ButtonProps) {
  return (
    <button
      {...props}
      // A button inside a form submits only when asked to (type="submit").
      type={type}
      className={cx(buttonClass({ variant, size, block }), loading && styles.loading, className)}
      disabled={disabled}
      aria-busy={loading || undefined}
      // aria-disabled keeps a loading button focusable (focus doesn't jump away mid-submit).
      aria-disabled={loading || undefined}
      onClick={(e) => {
        if (loading) {
          e.preventDefault();
          return;
        }
        onClick?.(e);
      }}
    >
      {loading ? (
        <span className={styles.icon}>
          <Spinner size="sm" label={null} />
        </span>
      ) : (
        icon && (
          <span className={styles.icon} aria-hidden="true">
            {icon}
          </span>
        )
      )}
      <span className={styles.label}>{children}</span>
    </button>
  );
}
