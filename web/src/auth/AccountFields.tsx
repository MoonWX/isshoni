import { useTranslation } from 'react-i18next';

import type { AccountRules } from '../protocol/api.gen';
import { TextField } from '../ui/Field';
import { checkNewPassword, checkUsername, fieldCodes, type FieldCodes } from './formErrors';
import { PasswordField } from './PasswordField';
import { useAccountRules } from './useSubmit';

export interface AccountValues {
  username: string;
  password: string;
}

/** The checks a new account's fields get before the request: required and the lengths of /info's accountRules. */
export function checkAccount(values: AccountValues, rules: AccountRules): FieldCodes<'username' | 'password'> {
  return fieldCodes({
    username: checkUsername(values.username, rules),
    password: checkNewPassword(values.password, rules),
  });
}

export interface AccountFieldsProps {
  values: AccountValues;
  onChange: (values: AccountValues) => void;
  /** Translated messages (useSubmit()'s fieldErrors). */
  errors: Partial<Record<'username' | 'password', string>>;
}

/**
 * The two fields of a new account (invite, sign-up, the first admin): a username and a password with show/hide and
 * no "repeat" field, each with its rule from /info's accountRules as a hint (05 §14.1, §15.1).
 */
export function AccountFields({ values, onChange, errors }: AccountFieldsProps) {
  const { t } = useTranslation();
  const rules = useAccountRules();
  return (
    <>
      <TextField
        label={t('auth.username')}
        name="username"
        autoComplete="username"
        autoCapitalize="none"
        autoCorrect="off"
        spellCheck={false}
        required
        value={values.username}
        onChange={(e) => {
          onChange({ ...values, username: e.target.value });
        }}
        hint={t('auth.usernameHint', { min: rules.usernameMinLength, max: rules.usernameMaxLength })}
        error={errors.username}
      />
      <PasswordField
        label={t('auth.password')}
        name="password"
        autoComplete="new-password"
        required
        value={values.password}
        onChange={(e) => {
          onChange({ ...values, password: e.target.value });
        }}
        hint={t('auth.passwordHint', { min: rules.passwordMinLength })}
        error={errors.password}
      />
    </>
  );
}
