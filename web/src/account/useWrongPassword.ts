// 403 wrong_password (03 §12.2: "re-authentication failed") on the account page's two forms: changing the password
// asks for the current one, deleting the account for the password. The code is about one field, so its text
// ("Your current password isn't right.") goes under that field and the field takes focus with its content selected,
// ready to be typed again. auth/useSubmit.ts puts only validation_failed and username_taken under fields; this is
// its `onError` for the one more.
import { useCallback, useEffect, useRef, useState, type RefObject } from 'react';
import { useTranslation } from 'react-i18next';

import { errorMessage } from '../lib/errorText';
import { CodeWrongPassword } from '../protocol/api.gen';
import { isApiError } from '../protocol/rest';

export interface WrongPassword {
  /** Goes on the password input that re-authenticates. */
  readonly inputRef: RefObject<HTMLInputElement | null>;
  /** The translated message under that field; undefined when the last submit wasn't refused for the password. */
  readonly error: string | undefined;
  /** useSubmit()'s onError: handles wrong_password (returns true), leaves every other failure to the form. */
  readonly onError: (err: unknown) => boolean;
  /** Call when a submit starts: the message belongs to the attempt before. */
  readonly clear: () => void;
}

export function useWrongPassword(): WrongPassword {
  const { t } = useTranslation();
  const inputRef = useRef<HTMLInputElement>(null);
  const [error, setError] = useState<string | undefined>(undefined);
  /** Counts refusals, so focus moves once per refused submit. */
  const [refusals, setRefusals] = useState(0);

  useEffect(() => {
    if (refusals === 0) return;
    inputRef.current?.focus();
    inputRef.current?.select();
  }, [refusals]);

  const onError = useCallback(
    (err: unknown): boolean => {
      if (!isApiError(err, CodeWrongPassword)) return false;
      setError(errorMessage(err, t));
      setRefusals((n) => n + 1);
      return true;
    },
    [t],
  );
  const clear = useCallback(() => {
    setError(undefined);
  }, []);

  return { inputRef, error, onError, clear };
}
