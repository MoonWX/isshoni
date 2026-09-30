import { Check } from 'lucide-react';
import { useTranslation } from 'react-i18next';

import { cx } from './cx';
import styles from './Stepper.module.css';
import { VisuallyHidden } from './VisuallyHidden';

export interface StepperProps {
  /** Step names in order (translated). */
  steps: readonly string[];
  /** The current step, 0-based. Earlier steps show as done. */
  current: number;
  /** The list's accessible name (translated), e.g. "Setup". */
  label: string;
}

type StepState = 'done' | 'current' | 'todo';

function stepState(i: number, current: number): StepState {
  if (i < current) return 'done';
  return i === current ? 'current' : 'todo';
}

/** Progress through a fixed sequence (the setup wizard's three steps, 05 §14.1). */
export function Stepper({ steps, current, label }: StepperProps) {
  const { t } = useTranslation();
  return (
    <ol className={styles.stepper} aria-label={label}>
      {steps.map((name, i) => {
        const state = stepState(i, current);
        return (
          <li
            key={name}
            className={cx(styles.step, styles[state])}
            aria-current={state === 'current' ? 'step' : undefined}
          >
            <span className={styles.marker} aria-hidden="true">
              {state === 'done' ? <Check /> : i + 1}
            </span>
            <span className={styles.name}>
              <VisuallyHidden>
                {state === 'done'
                  ? t('a11y.stepDoneOf', { current: i + 1, total: steps.length })
                  : t('a11y.stepOf', { current: i + 1, total: steps.length })}{' '}
              </VisuallyHidden>
              {name}
            </span>
          </li>
        );
      })}
    </ol>
  );
}
