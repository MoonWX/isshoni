// The dialog of one admin action (05 §15.3): what is about to happen, the fields it needs (a new name, the admin's
// own password), and Cancel beside the button that does it. Enter in a field submits; Esc and Cancel close.
//
// While the request is on its way the dialog can't be left: no Cancel, no ✕, no Esc, no click beside it. The change
// can't be taken back anymore, and its answer belongs to this dialog: a one-time link that must be shown, or a
// "done" that would otherwise close whatever dialog the admin opened next. The wait ends when the request settles
// (useAction's requests run at once and are never retried).
import { useEffect, useId, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';

import { Notice } from '../auth/Notice';
import { Button } from '../ui/Button';
import { Dialog } from '../ui/Dialog';
import styles from './ActionDialog.module.css';
import type { Action } from './useAction';

export interface ActionDialogProps<F extends string, V> {
  /** The heading (translated): the question, or what the form does. */
  title: string;
  /** useAction()'s result: its busy state, and the message above the fields. */
  action: Pick<Action<F, V>, 'formRef' | 'busy' | 'formError'>;
  onSubmit: () => void;
  onClose: () => void;
  /** The button that does it (translated). */
  submitLabel: string;
  /** A destructive action: the button is red. */
  danger?: boolean;
  /** sm: a confirmation; md: a form. */
  size?: 'sm' | 'md';
  /** The explanation and the fields. */
  children?: ReactNode;
}

/** Mount it while the action is being asked about; it opens at once and unmounts when done or cancelled. */
export function ActionDialog<F extends string, V>({
  title,
  action,
  onSubmit,
  onClose,
  submitLabel,
  danger = false,
  size = 'sm',
  children,
}: ActionDialogProps<F, V>) {
  const { t } = useTranslation();
  const formId = useId();
  const { formRef } = action;
  // showModal() puts focus on the dialog's first control, the ✕ button. A dialog that asks for something starts in
  // its first field instead; a plain confirmation keeps the safe default. (Dialog's own effect ran before this one.)
  useEffect(() => {
    formRef.current?.querySelector<HTMLElement>('input, select, textarea')?.focus();
  }, [formRef]);
  return (
    <Dialog
      open
      onClose={onClose}
      dismissible={!action.busy}
      title={title}
      size={size}
      footer={
        <>
          <Button onClick={onClose} disabled={action.busy}>
            {t('common.cancel')}
          </Button>
          {/* Outside the <form> in the DOM (the dialog's footer), tied to it by the form attribute. */}
          <Button type="submit" form={formId} variant={danger ? 'danger' : 'primary'} loading={action.busy}>
            {submitLabel}
          </Button>
        </>
      }
    >
      <form
        id={formId}
        ref={formRef}
        className={styles.form}
        noValidate
        onSubmit={(e) => {
          e.preventDefault();
          onSubmit();
        }}
      >
        {action.formError !== null && <Notice>{action.formError}</Notice>}
        {children}
      </form>
    </Dialog>
  );
}

/** A paragraph of the dialog's explanation. */
export function DialogText({ children }: { children: ReactNode }) {
  return <p className={styles.text}>{children}</p>;
}
