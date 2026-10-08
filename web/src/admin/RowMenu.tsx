// A row's actions behind one "⋯" button (the users list has up to six per row): a ui/Popover with a list of
// buttons. Choosing one closes the panel and puts focus back on the "⋯" button first, so the dialog that the action
// opens returns focus there when it closes (the browser restores it to what had focus when showModal() ran).
import { Ellipsis } from 'lucide-react';
import { useId } from 'react';

import { cx } from '../ui/cx';
import { Popover } from '../ui/Popover';
import styles from './RowMenu.module.css';

export interface RowMenuItem {
  /** Translated. */
  label: string;
  onSelect: () => void;
  /** A destructive action: shown last, in the danger colour. */
  danger?: boolean;
}

export interface RowMenuProps {
  /** The button's accessible name (translated): "Actions for sam". */
  label: string;
  items: readonly RowMenuItem[];
}

export function RowMenu({ label, items }: RowMenuProps) {
  const triggerId = useId();
  if (items.length === 0) return null;
  return (
    <Popover
      label={label}
      align="end"
      trigger={(props) => (
        <button {...props} id={triggerId} type="button" className={styles.trigger} aria-label={label}>
          <Ellipsis aria-hidden="true" />
        </button>
      )}
    >
      {(close) => (
        <ul className={styles.list}>
          {items.map((item) => (
            <li key={item.label}>
              <button
                type="button"
                className={cx(styles.item, item.danger === true && styles.danger)}
                onClick={() => {
                  close();
                  document.getElementById(triggerId)?.focus();
                  item.onSelect();
                }}
              >
                {item.label}
              </button>
            </li>
          ))}
        </ul>
      )}
    </Popover>
  );
}
