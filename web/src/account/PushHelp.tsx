// What a device that can't turn notifications on right now is told (05 §16.3), on the notifications page and in
// the room's Home Screen sheet:
// - `needs-install` (iOS or iPadOS in a browser tab): Share → Add to Home Screen, open it from there, log in once
//   more inside the app (iOS Home Screen apps have their own cookie jar, 03 §7.4), and the line for chat apps whose
//   built-in browser platform.inAppBrowser() can't detect;
// - `denied`: how to allow notifications again in the browser's settings;
// - `unsupported`: why. The server has no push (/info without `push`), an iOS without the Push API (an installed app
//   before iOS 16.4), or another browser that can't.
import { Share, SquarePlus } from 'lucide-react';
import { useTranslation } from 'react-i18next';

import { useApp } from '../app/context';
import { useInfo } from '../app/info';
import { ClientOSIOS } from '../protocol/types.gen';
import styles from './PushHelp.module.css';

/** The steps from a Safari tab to notifications in the Home Screen app. */
export function HomeScreenSteps() {
  const { t } = useTranslation();
  return (
    <div className={styles.help}>
      <p>{t('account.push.homeScreen.lead')}</p>
      <ol className={styles.steps} aria-label={t('account.push.homeScreen.stepsLabel')}>
        <li>
          <Share aria-hidden="true" />
          <span>{t('account.push.homeScreen.step1')}</span>
        </li>
        <li>
          <SquarePlus aria-hidden="true" />
          <span>{t('account.push.homeScreen.step2')}</span>
        </li>
        <li>
          <span className={styles.number} aria-hidden="true" />
          <span>{t('account.push.homeScreen.step3')}</span>
        </li>
      </ol>
      <p>{t('account.push.homeScreen.login')}</p>
      <p className={styles.muted}>{t('account.push.homeScreen.chatApp')}</p>
    </div>
  );
}

/** Why notifications can't be turned on here: the server, the iOS version, or the browser. */
export function UnsupportedText() {
  const { t } = useTranslation();
  const { platform } = useApp();
  const info = useInfo().data;
  if (info !== undefined && info.push === undefined) return <p>{t('account.push.unsupported.server')}</p>;
  if (platform.client.os === ClientOSIOS) return <p>{t('account.push.unsupported.ios')}</p>;
  return <p>{t('account.push.unsupported.browser')}</p>;
}

/** The help for a support state other than `supported`. */
export function PushHelp({ support }: { support: 'needs-install' | 'denied' | 'unsupported' }) {
  const { t } = useTranslation();
  switch (support) {
    case 'needs-install':
      return <HomeScreenSteps />;
    case 'denied':
      return <p>{t('account.push.denied')}</p>;
    case 'unsupported':
      return <UnsupportedText />;
  }
}
