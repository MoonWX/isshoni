// One submit flow for the auth and setup forms (05 §15.1, §14.1): check the fields against /info's account rules,
// send the request, then either finish (the page navigates) or show what the server's code means (formErrors.ts).
// After a failed submit, focus moves to the first invalid control (05 §16.6; ui/Field.tsx marks it aria-invalid);
// a message above the form is announced by its role="alert" instead.
import { useMutation } from '@tanstack/react-query';
import { useEffect, useRef, useState, type RefObject } from 'react';
import { useTranslation } from 'react-i18next';

import { useInfo } from '../app/info';
import type { AccountRules } from '../protocol/api.gen';
import {
  DEFAULT_ACCOUNT_RULES,
  noProblem,
  problemFromCodes,
  problemFromError,
  waitMessage,
  type FieldCodes,
  type FormProblem,
} from './formErrors';
import { useRetryWait } from './useRetryWait';

/** The account rules of GET /api/v1/info (03 §12.4.1), which boot loaded. */
export function useAccountRules(): AccountRules {
  return useInfo().data?.accountRules ?? DEFAULT_ACCOUNT_RULES;
}

export interface SubmitOptions<F extends string, V, R> {
  /** The form's fields by their JSON names in the request (03 §12.4.2), in the order they appear. */
  fields: readonly F[];
  /** Checks before anything is sent: the fields that fail, with their field code (03 §12.2). */
  validate?: (values: V) => FieldCodes<F>;
  /** The request. */
  request: (values: V) => Promise<R>;
  /** After a 2xx: start the session, navigate. The form stays busy until this settles; a rejection shows as an error. */
  onSuccess: (result: R) => void | Promise<void>;
  /**
   * A failure the page handles itself (a link that died, account_pending → /pending). Return true when handled:
   * the form then shows nothing for it.
   */
  onError?: (err: unknown) => boolean;
}

export interface Submit<F extends string, V> {
  /** Put it on the <form>. */
  readonly formRef: RefObject<HTMLFormElement | null>;
  submit(values: V): void;
  /** The request is running, or it succeeded and the page is about to change: the submit button shows a spinner. */
  readonly busy: boolean;
  /** Translated messages per field. */
  readonly fieldErrors: Partial<Record<F, string>>;
  /** The translated message above the form, counted down while the server asked to wait; null when there is none. */
  readonly formError: string | null;
  /**
   * Set while formError counts down: the message as it was when the wait began, which screen readers hear once
   * (Notice's `spoken`) instead of every tick.
   */
  readonly formErrorSpoken: string | undefined;
  /** The server asked to wait (rate_limited, server_busy): the submit button is off until it is over. */
  readonly waiting: boolean;
}

export function useSubmit<F extends string, V, R>(opts: SubmitOptions<F, V, R>): Submit<F, V> {
  const { t } = useTranslation();
  const formRef = useRef<HTMLFormElement>(null);
  const [problem, setProblem] = useState<FormProblem<F>>(noProblem);
  /** Counts failed submits, so focus moves once per failure. */
  const [failures, setFailures] = useState(0);
  const wait = useRetryWait();

  const fail = (p: FormProblem<F>): void => {
    setProblem(p);
    setFailures((n) => n + 1);
    if (p.wait) wait.start(p.wait.seconds);
  };

  const mutation = useMutation({
    // The values (a password among them) are not kept after the form unmounts.
    gcTime: 0,
    mutationFn: async (values: V) => {
      await opts.onSuccess(await opts.request(values));
    },
    onError: (err) => {
      if (opts.onError?.(err) === true) {
        setProblem(noProblem());
        return;
      }
      fail(problemFromError(err, opts.fields, t));
    },
  });

  useEffect(() => {
    if (failures === 0) return;
    formRef.current?.querySelector<HTMLElement>('[aria-invalid="true"]')?.focus();
  }, [failures]);

  const busy = mutation.isPending || mutation.isSuccess;
  const waiting = wait.secondsLeft > 0;

  return {
    formRef,
    submit(values) {
      if (busy || waiting) return;
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
    // While the wait runs the message counts down; when it is over the message goes and the button comes back.
    formError: problem.wait ? (waiting ? waitMessage(t, problem.wait.code, wait.secondsLeft) : null) : problem.form,
    formErrorSpoken: problem.wait && waiting ? (problem.form ?? undefined) : undefined,
    waiting,
  };
}
