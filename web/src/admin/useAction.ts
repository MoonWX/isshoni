// One submit flow for the admin pages' forms and confirmation dialogs (05 §15.3): check the fields, send the change,
// then either finish (the page closes the dialog, refreshes its list) or show what the server's code means
// (formProblem.ts). After a failed submit, focus moves to the first invalid control (05 §16.6; ui/Field.tsx marks
// it aria-invalid); a message above the form is announced by its role="alert" instead.
import { useMutation } from '@tanstack/react-query';
import { useEffect, useRef, useState, type RefObject } from 'react';
import { useTranslation } from 'react-i18next';

import {
  noProblem,
  problemFromCodes,
  problemFromError,
  type CodeFields,
  type FieldCodes,
  type FormProblem,
} from './formProblem';

export interface ActionOptions<F extends string, V, R> {
  /** The form's fields by their JSON names in the request, in the order they appear. None for a plain confirmation. */
  fields?: readonly F[];
  /** Checks before anything is sent: the fields that fail, with their field code (03 §12.2). */
  validate?: (values: V) => FieldCodes<F>;
  /** The request. */
  request: (values: V) => Promise<R>;
  /** After a 2xx. The form stays busy until this settles. */
  onSuccess: (result: R, values: V) => void | Promise<void>;
  /** The error codes that are about one of the fields: {wrong_password: 'currentPassword'}. */
  codeFields?: CodeFields<F>;
  /** After a failure, whatever it was (refresh a list that turned out to be stale). */
  onError?: (err: unknown) => void;
}

export interface Action<F extends string, V> {
  /** Put it on the <form>. */
  readonly formRef: RefObject<HTMLFormElement | null>;
  submit(values: V): void;
  /** The request is running: the submit button shows a spinner. */
  readonly busy: boolean;
  /** Translated messages per field. */
  readonly fieldErrors: Partial<Record<F, string>>;
  /** The translated message above the form; null when there is none. */
  readonly formError: string | null;
  /**
   * Forgets the last failure (the form is used for the next item), or with a field only that field's message (it
   * is being edited).
   */
  clear(field?: F): void;
}

const NO_FIELDS: readonly never[] = [];

export function useAction<V, R, F extends string = never>(opts: ActionOptions<F, V, R>): Action<F, V> {
  const { t } = useTranslation();
  const formRef = useRef<HTMLFormElement>(null);
  const [problem, setProblem] = useState<FormProblem<F>>(noProblem);
  /** Counts failed submits, so focus moves once per failure. */
  const [failures, setFailures] = useState(0);

  const fail = (p: FormProblem<F>): void => {
    setProblem(p);
    setFailures((n) => n + 1);
  };

  const mutation = useMutation({
    // The values (the admin's password among them) are not kept after the form unmounts.
    gcTime: 0,
    mutationFn: async (values: V) => {
      await opts.onSuccess(await opts.request(values), values);
    },
    onError: (err) => {
      fail(problemFromError(err, opts.fields ?? NO_FIELDS, opts.codeFields ?? {}, t));
      opts.onError?.(err);
    },
  });

  useEffect(() => {
    if (failures === 0) return;
    formRef.current?.querySelector<HTMLElement>('[aria-invalid="true"]')?.focus();
  }, [failures]);

  const busy = mutation.isPending;

  return {
    formRef,
    submit(values) {
      if (busy) return;
      const codes = opts.validate?.(values) ?? {};
      if (Object.keys(codes).length > 0) {
        fail(problemFromCodes(codes, t));
        return;
      }
      setProblem(noProblem());
      mutation.mutate(values);
    },
    busy,
    fieldErrors: problem.fields,
    formError: problem.form,
    clear(field) {
      if (field === undefined) {
        setProblem(noProblem());
        return;
      }
      setProblem((p) => {
        if (!Object.hasOwn(p.fields, field)) return p;
        const rest = Object.entries(p.fields).filter(([name]) => name !== field);
        return { ...p, fields: Object.fromEntries(rest) as Partial<Record<F, string>> };
      });
    },
  };
}
