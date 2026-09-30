import { LockOpen } from 'lucide-react';
import { useTranslation } from 'react-i18next';

import { ScreenFrame } from './ScreenFrame';

/**
 * The page isn't a secure context (05 §4 step 1): getDisplayMedia, the service worker, push and wake lock all need
 * one. Only the admin can fix it, so there is no action.
 */
export function NeedsHttps() {
  const { t } = useTranslation();
  return (
    <ScreenFrame icon={LockOpen} tone="warning" title={t('common.screens.needsHttps.title')}>
      <p>{t('common.screens.needsHttps.body')}</p>
    </ScreenFrame>
  );
}
