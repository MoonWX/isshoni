// A new invite's link, shown the one time the server reveals it (05 §14.1, 03 §7.9): the link with Copy, a QR code
// for a phone, Share… where the browser has a share sheet, and how long and for how many people it works. The
// server keeps only the token's hash, so the card says that the link can't be shown again and where to make another.
//
// The wizard's step 3 shows it (WelcomePage). It is not a page, so setup/index.ts doesn't export it: another folder
// that wants it (the room's account menu, 05 §11.2) imports this file. Its texts are in the lazy `setup` namespace,
// so that folder's entry module imports i18n/lazy/setup too (05 §16.5; check:i18n names the file that misses it).
//
// The link lives in the caller's state until the admin is done with it: never in the REST cache, never in storage
// (05 §20). The clipboard is written only on a click.
import { Check, Copy, Share } from 'lucide-react';
import { useEffect, useRef, useState } from 'react';
import { Trans, useTranslation } from 'react-i18next';
import { Link } from 'react-router';

import { useApp } from '../app/context';
import { useInfo } from '../app/info';
import { copyText } from '../lib/clipboard';
import { createLogger } from '../lib/log';
import { formatDateTime, parseWireTime } from '../lib/time';
import type { CreateInviteResponse } from '../protocol/api.gen';
import { Button } from '../ui/Button';
import { Field, inputClass } from '../ui/Field';
import { cx } from '../ui/cx';
import styles from './InviteLinkCard.module.css';
import { QrCode } from './QrCode';

const log = createLogger('setup');

export interface InviteLinkCardProps {
  /** POST /api/v1/invites's answer (03 §12.4.5): the link, and the invite with its expiry and its uses. */
  created: CreateInviteResponse;
  className?: string;
}

/** What the line under the link says after a click: the copy worked, or the link must be copied by hand. */
type Feedback = 'idle' | 'copied' | 'copyFailed' | 'shareFailed';

/** navigator.share where the browser has a share sheet (phones, Safari, Chromium on Windows and ChromeOS). */
function shareFunction(): ((data: ShareData) => Promise<void>) | null {
  const nav = globalThis.navigator as Partial<Pick<Navigator, 'share'>> | undefined;
  return typeof nav?.share === 'function' ? nav.share.bind(nav) : null;
}

function isAbort(err: unknown): boolean {
  return err instanceof DOMException && err.name === 'AbortError';
}

export function InviteLinkCard({ created, className }: InviteLinkCardProps) {
  const { t, i18n } = useTranslation();
  const { ui } = useApp();
  const serverName = useInfo().data?.server.name ?? '';
  const input = useRef<HTMLInputElement>(null);
  const [feedback, setFeedback] = useState<Feedback>('idle');
  const { url, invite } = created;
  const share = shareFunction();
  const expires = parseWireTime(invite.expiresAt);

  useEffect(() => {
    if (feedback !== 'copied') return undefined;
    const id = setTimeout(() => {
      setFeedback('idle');
    }, 2000);
    return () => {
      clearTimeout(id);
    };
  }, [feedback]);

  /** The browser refused: the link is selected, ready for the user's own copy. */
  const selectLink = (): void => {
    input.current?.focus();
    input.current?.select();
  };

  const copy = async (): Promise<void> => {
    if (await copyText(url)) {
      setFeedback('copied');
      ui.getState().announce(t('setup.inviteCard.copiedSpoken'));
      return;
    }
    setFeedback('copyFailed');
    selectLink();
  };

  const shareLink = async (): Promise<void> => {
    if (!share) return;
    try {
      await share({
        title: t('setup.inviteCard.shareTitle', { server: serverName }),
        text: t('setup.inviteCard.shareText', { server: serverName }),
        url,
      });
      setFeedback('idle');
    } catch (err) {
      // The user closed the share sheet: nothing to say.
      if (isAbort(err)) return;
      log.warn('the share sheet failed', { error: err instanceof Error ? err.name : 'unknown' });
      setFeedback('shareFailed');
      selectLink();
    }
  };

  let status: string | null = null;
  if (feedback === 'copyFailed') status = t('setup.inviteCard.copyManually');
  else if (feedback === 'shareFailed') status = t('setup.inviteCard.shareFailed');

  return (
    <div className={cx(styles.card, className)}>
      <div className={styles.link}>
        <Field label={t('setup.inviteCard.label')}>
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
                icon={feedback === 'copied' ? <Check /> : <Copy />}
                onClick={() => {
                  void copy();
                }}
              >
                {feedback === 'copied' ? t('setup.inviteCard.copied') : t('setup.inviteCard.copy')}
              </Button>
            </div>
          )}
        </Field>
        {/* Always there, so screen readers hear the text when it arrives. */}
        <p className={styles.status} role="status">
          {status}
        </p>
      </div>

      <div className={styles.more}>
        <figure className={styles.qr}>
          <QrCode value={url} label={t('setup.inviteCard.qr')} size={176} />
          <figcaption className={styles.qrHint}>{t('setup.inviteCard.qrHint')}</figcaption>
        </figure>
        <div className={styles.facts}>
          {expires !== undefined && (
            <p>
              {t('setup.inviteCard.expiry', {
                count: invite.maxUses,
                date: formatDateTime(expires, i18n.language),
              })}
            </p>
          )}
          <p className={styles.once}>
            <Trans i18nKey="setup.inviteCard.once" components={{ a: <Link to="/admin/invites" /> }} />
          </p>
          {share && (
            <Button
              icon={<Share />}
              onClick={() => {
                void shareLink();
              }}
            >
              {t('setup.inviteCard.share')}
            </Button>
          )}
        </div>
      </div>
    </div>
  );
}
