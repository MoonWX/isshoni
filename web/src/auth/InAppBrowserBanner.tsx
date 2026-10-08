import { Check, Copy, ExternalLink, X } from 'lucide-react';
import { useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';

import { useApp } from '../app/context';
import { copyText } from '../lib/clipboard';
import { Button } from '../ui/Button';
import { TextField } from '../ui/Field';
import { externalBrowser, inviteLink, useInAppBanner } from './inAppBanner';
import styles from './InAppBrowserBanner.module.css';

export interface InAppBrowserBannerProps {
  /**
   * The invite page's token (fragmentToken.ts keeps it in memory): Copy link then gives `<origin>/invite#<token>`,
   * because boot removed the fragment from the address bar. Every other page leaves it out and copies the address
   * as it is.
   */
  inviteToken?: string | null;
}

/** How long the button says "Copied". */
const COPIED_MS = 2000;

/**
 * The in-app browser banner (05 §16.3): shown above the content of the invite, login and room pages when the page
 * runs in a chat or social app's built-in browser (platform.inAppBrowser()), until it is dismissed for the tab.
 * "For notifications and the Home Screen app, open this link in Safari" (Chrome on Android, "your browser"
 * elsewhere), with Copy link. When the clipboard refuses (in-app browsers often do), the link shows in a selected
 * read-only field instead. In Safari, Chrome and every other real browser it renders nothing.
 */
export function InAppBrowserBanner({ inviteToken }: InAppBrowserBannerProps) {
  const { t } = useTranslation();
  const { platform, ui } = useApp();
  const banner = useInAppBanner();
  const [copy, setCopy] = useState<'idle' | 'copied' | 'manual'>('idle');
  const field = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (copy !== 'copied') return undefined;
    const id = setTimeout(() => {
      setCopy('idle');
    }, COPIED_MS);
    return () => {
      clearTimeout(id);
    };
  }, [copy]);

  // The clipboard refused: the link is on the page now, selected, so the friend's own Copy works.
  useEffect(() => {
    const input = field.current;
    if (copy !== 'manual' || !input) return;
    input.focus();
    // iOS selects through setSelectionRange only.
    input.setSelectionRange(0, input.value.length);
  }, [copy]);

  if (!banner.showing) return null;

  const link =
    typeof inviteToken === 'string' && inviteToken !== ''
      ? inviteLink(platform.serverOrigin, inviteToken)
      : globalThis.location.href;
  const browser = externalBrowser(platform.client.os);

  return (
    <section className={styles.banner} aria-label={t('auth.inAppBrowser.label')}>
      <ExternalLink className={styles.icon} aria-hidden="true" />
      <div className={styles.body}>
        <p className={styles.text}>
          {browser === 'safari'
            ? t('auth.inAppBrowser.safari')
            : browser === 'chrome'
              ? t('auth.inAppBrowser.chrome')
              : t('auth.inAppBrowser.other')}
        </p>
        {copy === 'manual' && (
          <TextField
            ref={field}
            className={styles.field}
            label={t('auth.inAppBrowser.manual')}
            value={link}
            readOnly
            // A link is not prose: no keyboard helpers, and a tap selects all of it again.
            autoCapitalize="none"
            autoCorrect="off"
            spellCheck={false}
            onFocus={(e) => {
              e.currentTarget.setSelectionRange(0, e.currentTarget.value.length);
            }}
          />
        )}
        <Button
          size="sm"
          icon={copy === 'copied' ? <Check /> : <Copy />}
          // The clipboard is written only on a click (05 §20).
          onClick={() => {
            void copyText(link).then((ok) => {
              setCopy(ok ? 'copied' : 'manual');
              if (ok) ui.getState().announce(t('auth.inAppBrowser.copied'));
            });
          }}
        >
          {copy === 'copied' ? t('common.copied') : t('auth.inAppBrowser.copy')}
        </Button>
      </div>
      <button type="button" className={styles.dismiss} aria-label={t('a11y.dismiss')} onClick={banner.dismiss}>
        <X aria-hidden="true" />
      </button>
    </section>
  );
}
