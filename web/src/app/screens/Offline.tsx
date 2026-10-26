import { WifiOff } from 'lucide-react';
import { useTranslation } from 'react-i18next';

import { Button } from '../../ui/Button';
import { ScreenFrame } from './ScreenFrame';

export interface OfflineProps {
  /** Try again now (the screen also retries by itself: on `online` and every 10 s, 05 §4). */
  onRetry: () => void;
  /** A try is running. */
  retrying: boolean;
  /** The desktop app (M2, docs/m2/03 §7.2): "Switch server", to Settings → Servers. The browser has none. */
  onSwitchServer?: () => void;
}

/** Boot couldn't reach GET /api/v1/info (05 §4). */
export function Offline({ onRetry, retrying, onSwitchServer }: OfflineProps) {
  const { t } = useTranslation();
  return (
    <ScreenFrame
      icon={WifiOff}
      title={t('common.screens.offline.title')}
      actions={
        <>
          <Button variant="primary" loading={retrying} onClick={onRetry}>
            {retrying ? t('common.screens.offline.retrying') : t('common.screens.offline.retry')}
          </Button>
          {onSwitchServer !== undefined && (
            <Button variant="secondary" onClick={onSwitchServer}>
              {t('common.screens.offline.switchServer')}
            </Button>
          )}
        </>
      }
    >
      <p>{t('common.screens.offline.body')}</p>
    </ScreenFrame>
  );
}
