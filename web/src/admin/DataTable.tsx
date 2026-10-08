// The admin lists (users, invites, rooms, the audit log): a real <table>, so screen readers announce each cell with
// its column. On a phone the same table reads as a stack of cards: each row a card, each cell a line that starts
// with its column's name (the CSS shows it from data-label), and nothing scrolls sideways.
import { createContext, useContext, type ReactNode } from 'react';

import { cx } from '../ui/cx';
import { VisuallyHidden } from '../ui/VisuallyHidden';
import styles from './DataTable.module.css';

const Columns = createContext<readonly string[]>([]);

export interface DataTableProps {
  /** What the table lists (translated): its accessible name. */
  caption: string;
  /** The column headings (translated). The last may be '' for a column of row actions. */
  columns: readonly string[];
  /**
   * Where a row's actions go in the phone layout: 'end' closes the card with its buttons; 'corner' puts a single
   * menu button beside the row's name.
   */
  actions?: 'end' | 'corner';
  /** <Row> elements. */
  children: ReactNode;
}

export function DataTable({ caption, columns, actions = 'end', children }: DataTableProps) {
  return (
    <table className={cx(styles.table, actions === 'corner' && styles.corner)}>
      <caption>
        <VisuallyHidden>{caption}</VisuallyHidden>
      </caption>
      <thead>
        <tr>
          {columns.map((name, i) =>
            // The headings are fixed per table, so the index is their identity. A column of actions has no heading.
            name === '' ? (
              <td key={i} />
            ) : (
              <th key={i} scope="col">
                {name}
              </th>
            ),
          )}
        </tr>
      </thead>
      <Columns.Provider value={columns}>
        <tbody>{children}</tbody>
      </Columns.Provider>
    </table>
  );
}

export interface RowProps {
  /**
   * One cell per column, in the table's column order. The first names the row (a <th scope="row">). A cell without
   * a value is null: the phone layout leaves its line out.
   */
  cells: readonly ReactNode[];
}

export function Row({ cells }: RowProps) {
  const columns = useContext(Columns);
  return (
    <tr>
      {cells.map((cell, i) => {
        // Cells are positional, one per column, so the index is their identity too.
        const label = columns[i] ?? '';
        if (i === 0) {
          return (
            <th key={i} scope="row" data-label={label}>
              {cell}
            </th>
          );
        }
        const empty = cell === null || cell === undefined || cell === false || cell === '';
        // One wrapper per cell, so the phone layout has exactly two parts to place: the label and the value.
        return (
          <td key={i} data-label={label} className={cx(label === '' && styles.actions, empty && styles.empty)}>
            <div className={label === '' ? styles.buttons : styles.value}>{cell}</div>
          </td>
        );
      })}
    </tr>
  );
}

/** Two short lines in one cell: the value, and a detail under it. */
export function Lines({ children }: { children: ReactNode }) {
  return <span className={styles.lines}>{children}</span>;
}

/** A cell's small print: a date under a name, who used an invite. */
export function Note({ children }: { children: ReactNode }) {
  return <span className={styles.note}>{children}</span>;
}

/** A value with its tags beside it ("sam [You]"), wrapping as one group. */
export function Inline({ children }: { children: ReactNode }) {
  return <span className={styles.inline}>{children}</span>;
}
