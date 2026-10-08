// A link the server shows exactly once: an invite link at creation (03 §7.9), a password-reset link (03 §7.10).
// Only its hash is stored, so a lost link means making a new one, and the text under it says so. The link lives in
// the page's state until the admin is done with it: never in the REST cache, never in storage (05 §20).
import { Check, Copy } from 'lucide-react';
import { useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';

import { useApp } from '../app/context';
import { copyText } from '../lib/clipboard';
import { Button } from '../ui/Button';
import { Field, inputClass } from '../ui/Field';
import { cx } from '../ui/cx';
import styles from './OneTimeLink.module.css';

export interface OneTimeLinkProps {
  /** The field's label (translated): "Invite link". */
  label: string;
  url: string;
  /** What to do with it, and that it won't be shown again (translated). */
  hint: string;
}

type CopyState = 'idle' | 'copied' | 'failed';

export function OneTimeLink({ label, url, hint }: OneTimeLinkProps) {
  const { t } = useTranslation();
  const { ui } = useApp();
  const input = useRef<HTMLInputElement>(null);
  const [state, setState] = useState<CopyState>('idle');

  useEffect(() => {
    if (state !== 'copied') return undefined;
    const id = setTimeout(() => {
      setState('idle');
    }, 2000);
    return () => {
      clearTimeout(id);
    };
  }, [state]);

  const copy = async (): Promise<void> => {
    if (await copyText(url)) {
      setState('copied');
      ui.getState().announce(t('admin.link.copied'));
      return;
    }
    // The browser refused (no permission, an insecure context): the link is selected, ready for the user's own copy.
    setState('failed');
    input.current?.focus();
    input.current?.select();
  };

  return (
    <div className={styles.link}>
      <Field label={label} hint={hint}>
        {(control) => (
          <div className={styles.row}>
            <input
              {...control}
              ref={input}
              className={cx(inputClass, styles.url)}
              type="text"
              readOnly
              value={url}
              // A link is not prose: no spelling service, no autocorrect.
              spellCheck={false}
              autoCapitalize="none"
              autoCorrect="off"
              onFocus={(e) => {
                e.currentTarget.select();
              }}
            />
            <Button
              variant="primary"
              icon={state === 'copied' ? <Check /> : <Copy />}
              onClick={() => {
                void copy();
              }}
            >
              {state === 'copied' ? t('admin.link.copied') : t('admin.link.copy')}
            </Button>
          </div>
        )}
      </Field>
      {/* Always there, so screen readers hear the text when it arrives. */}
      <p className={styles.failed} role="status">
        {state === 'failed' ? t('admin.link.copyManually') : null}
      </p>
    </div>
  );
}
