import { ExternalLink } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router';

import { usePlatform } from '../app/context';
import { CenteredPage } from '../app/layouts/CenteredPage';
import { buttonClass } from '../ui/Button';
import styles from './DownloadPage.module.css';

/**
 * The project site (06 D11), where the desktop app will be announced. The same address as auth/AboutPage.tsx's;
 * it is repeated here so that this page's chunk doesn't load the auth pages for one string.
 */
export const PROJECT_SITE_URL = 'https://moonwx.github.io/isshoni/';

/**
 * /download (05 §5, §15.4): the M1 placeholder. There is no desktop app yet, so the page says that one is coming
 * and what it will be for, how to share well from a browser until then, and where the project lives. Public: it
 * needs nothing from the server.
 *
 * later (M2): per-OS installers, the pre-filled install-desktop.sh/.ps1 commands (04 reserves the paths), and the
 * OS detected from ClientInfo.os.
 */
export function DownloadPage() {
  const { t } = useTranslation();
  const platform = usePlatform();
  return (
    <CenteredPage title={t('account.download.title')} footer={<Link to="/">{t('account.back')}</Link>}>
      <div className={styles.prose}>
        <p>{t('account.download.coming')}</p>
        <p>{t('account.download.untilThen')}</p>
      </div>
      <div className={styles.actions}>
        {/* Leaves the SPA: a new tab, without opener or referrer (05 §20). */}
        <a
          href={PROJECT_SITE_URL}
          target="_blank"
          rel="noopener noreferrer"
          className={buttonClass({ variant: 'secondary' })}
          onClick={(e) => {
            // Outside a browser tab (the desktop app, M2) the platform opens it in the system browser.
            if (platform.kind === 'browser') return;
            e.preventDefault();
            platform.openExternal(PROJECT_SITE_URL);
          }}
        >
          <span>{t('account.download.site')}</span>
          <ExternalLink aria-hidden="true" />
        </a>
      </div>
    </CenteredPage>
  );
}
