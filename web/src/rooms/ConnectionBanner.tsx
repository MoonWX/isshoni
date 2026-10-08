// The connection banner (05 §7.1): nothing while the connection is fine or for a blip under 2 s, then
// "Reconnecting…", after 30 s "Can't reach the server. Retrying… [Retry now] [Test my connection]", and "Server
// restarting…" after the server announced its shutdown. An aria-live="polite" region that stays mounted, so screen
// readers hear each text when it appears. The room page mounts it at its top, and so does whatever shows the
// session on other pages (InRoomBar): media keeps playing below it while the PCs are healthy.
import { ServerCog, WifiOff } from 'lucide-react';
import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';

import { Button } from '../ui/Button';
import { cx } from '../ui/cx';
import { Spinner } from '../ui/Spinner';
import styles from './ConnectionBanner.module.css';
import { connectionBanner, nextBannerChange, type ConnectionBannerState, type ConnectionStore } from './connection';
import { useRoomRuntime } from './hooks';

/** The banner of the store's state now, kept current by the store and by the clock (the 2 s and 30 s marks). */
export function useConnectionBanner(store: ConnectionStore): ConnectionBannerState | null {
  const [banner, setBanner] = useState(() => connectionBanner(store.getState(), Date.now()));
  useEffect(() => {
    let timer: ReturnType<typeof setTimeout> | undefined;
    const arm = (): void => {
      clearTimeout(timer);
      const wait = nextBannerChange(store.getState(), Date.now());
      timer = wait === null ? undefined : setTimeout(update, wait);
    };
    const update = (): void => {
      const next = connectionBanner(store.getState(), Date.now());
      setBanner((prev) => (prev?.kind === next?.kind && prev?.retryInSec === next?.retryInSec ? prev : next));
      arm();
    };
    arm();
    const off = store.subscribe(update);
    return () => {
      off();
      clearTimeout(timer);
    };
  }, [store]);
  return banner;
}

export interface ConnectionBannerProps {
  /**
   * Opens the connection test (05 §14.2). The "Test my connection" button shows only when this is given.
   */
  onTestConnection?: () => void;
}

export function ConnectionBanner({ onTestConnection }: ConnectionBannerProps) {
  const { t } = useTranslation();
  const { signal, stores } = useRoomRuntime();
  const banner = useConnectionBanner(stores.connection);
  return (
    <div className={styles.region} aria-live="polite" data-testid="connection-banner">
      {banner?.kind === 'reconnecting' && (
        <div className={styles.banner} data-kind="reconnecting">
          <Spinner size="sm" label={null} />
          <p className={styles.text}>{t('room.connection.reconnecting')}</p>
        </div>
      )}
      {banner?.kind === 'restarting' && (
        <div className={styles.banner} data-kind="restarting">
          <ServerCog className={styles.icon} aria-hidden="true" />
          <p className={styles.text}>{t('room.connection.restarting')}</p>
        </div>
      )}
      {banner?.kind === 'unreachable' && (
        <div className={cx(styles.banner, styles.unreachable)} data-kind="unreachable">
          <WifiOff className={styles.icon} aria-hidden="true" />
          <p className={styles.text}>{t('room.connection.unreachable')}</p>
          <div className={styles.actions}>
            <Button
              size="sm"
              // A rate-limit wait can't be skipped (01 §10.2): the button counts it down instead.
              disabled={banner.retryInSec !== null}
              onClick={() => {
                signal.retryNow();
              }}
            >
              {banner.retryInSec !== null
                ? t('room.connection.retryIn', { count: banner.retryInSec })
                : t('room.connection.retryNow')}
            </Button>
            {onTestConnection && (
              <Button size="sm" variant="ghost" onClick={onTestConnection}>
                {t('room.connection.testConnection')}
              </Button>
            )}
          </div>
        </div>
      )}
    </div>
  );
}
