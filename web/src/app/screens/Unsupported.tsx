import { MonitorX } from 'lucide-react';
import { useTranslation } from 'react-i18next';

import { ScreenFrame } from './ScreenFrame';

/**
 * No WebRTC (05 §4). /boot-check.js catches too-old browsers before the bundle loads; this screen covers a browser
 * that runs the bundle but has WebRTC missing or turned off (a policy, an extension, a privacy setting).
 */
export function Unsupported() {
  const { t } = useTranslation();
  return (
    <ScreenFrame icon={MonitorX} tone="danger" title={t('common.screens.unsupported.title')}>
      <p>{t('common.screens.unsupported.body')}</p>
    </ScreenFrame>
  );
}
