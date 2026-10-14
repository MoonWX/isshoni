// The codec wait's banner (05 §10.6), above the stage, for when waiting no longer helps:
// - after 3 minutes without a decoder: "Reload the page" with a Reload button, and in Firefox where to check its
//   OpenH264 plugin (a reload gives fresh caps and a fresh PC, which is what the server's retries need by then,
//   02 §8.5);
// - in a browser other than Firefox that has no H.264 decoder at all: "This browser can't play isshoni video";
//   the audio still plays.
// While the page still waits, the tiles say so themselves (shareView.ts); the wait is codecWait.ts's.
import { useContext } from 'react';
import { useTranslation } from 'react-i18next';
import { useStore } from 'zustand';

import { AppContext } from '../app/context';
import { Button } from '../ui/Button';
import { codecNotice } from './codecWait';
import type { ViewerStore } from './viewerStore';

export interface CodecBannerProps {
  store: ViewerStore;
  className?: string | undefined;
}

export function CodecBanner({ store, className }: CodecBannerProps) {
  const { t } = useTranslation();
  const app = useContext(AppContext);
  const notice = useStore(store, (s) => codecNotice(s.codecWait));
  if (notice === null || notice === 'waiting') return null;
  if (notice === 'missing') {
    return (
      <div className={className} role="status" data-codec-banner="missing">
        <p>{t('viewer.codec.missing')}</p>
      </div>
    );
  }
  const reload = (): void => {
    const platformReload = app?.platform.versionActions().reload;
    if (platformReload) platformReload();
    else globalThis.location.reload();
  };
  return (
    <div className={className} role="status" data-codec-banner="stuck">
      <p>{notice === 'stuckFirefox' ? t('viewer.codec.stuckFirefox') : t('viewer.codec.stuck')}</p>
      <Button onClick={reload}>{t('common.reload')}</Button>
    </div>
  );
}
