// The one-time hint on iOS (05 §12.8): "Keep isshoni open while watching. Video pauses when you switch apps."
// iOS runs WebRTC in the foreground only: it is suspended in the background and when the screen locks, so someone
// who switches apps comes back to a picture that has to start again (page.ts plays everything again then).
//
// It shows while the page shows someone's share, until "Got it": the dismissal is stored on the device
// (prefsStore.dismissed), so it never comes back. Whether this is iOS is the platform's client.os, which only ever
// chooses help text (05 §8). Outside the app's providers (a bare ViewerLayout in a test) there is no hint.
import { useContext } from 'react';
import { useTranslation } from 'react-i18next';
import { useStore } from 'zustand';

import { AppContext } from '../app/context';
import type { PrefsStore } from '../app/prefs';
import { ClientOSIOS } from '../protocol/types.gen';
import { Button } from '../ui/Button';
import { cx } from '../ui/cx';
import { useViewer } from './context';
import styles from './IosHint.module.css';
import { isWatching } from './wakeLock';

/** The hint's id among prefsStore's dismissals. */
export const IOS_HINT_ID = 'viewer.iosForeground';

export interface IosHintProps {
  className?: string | undefined;
}

/** Renders nothing off iOS, before a share is watched, and once it was dismissed. Inside a ViewerLayout. */
export function IosHint({ className }: IosHintProps) {
  const app = useContext(AppContext);
  if (app === null || (app.platform.client.os as string) !== ClientOSIOS) return null;
  return <Hint prefs={app.prefs} className={className} />;
}

function Hint({ prefs, className }: { prefs: PrefsStore; className?: string | undefined }) {
  const { t } = useTranslation();
  const dismissed = useStore(prefs, (s) => s.dismissed[IOS_HINT_ID] !== undefined);
  const watching = useViewer(isWatching);
  if (dismissed || !watching) return null;
  return (
    <div className={cx(styles.hint, className)} role="note">
      <p>{t('viewer.iosHint.text')}</p>
      <Button
        size="sm"
        onClick={() => {
          prefs.getState().dismiss(IOS_HINT_ID);
        }}
      >
        {t('viewer.iosHint.dismiss')}
      </Button>
    </div>
  );
}
