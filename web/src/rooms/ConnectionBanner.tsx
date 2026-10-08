// The connection banner (05 §7.1): nothing while the connection is fine or for a blip under 2 s, then
// "Reconnecting…", after 30 s "Can't reach the server. Retrying… [Retry now] [Test my connection]", and "Server
// restarting…" after the server announced its shutdown. Its text is an aria-live="polite" region that stays mounted,
// so screen readers hear each text when it appears; the buttons are outside it. The room page mounts it at its top,
// and so does whatever shows the session on other pages (InRoomBar): media keeps playing below it while the PCs are
// healthy.
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
    // Not just arm(): the first value is from render time, and the clock or the store may have moved on since (a
    // mark passed, the connection came back). Nothing else would tell: the next store change may be far away.
    update();
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
  const kind = banner?.kind;
  return (
    <div
      className={cx(styles.region, kind !== undefined && styles.banner, kind === 'unreachable' && styles.unreachable)}
      data-kind={kind}
      data-testid="connection-banner"
    >
      {kind === 'reconnecting' && <Spinner size="sm" label={null} />}
      {kind === 'restarting' && <ServerCog className={styles.icon} aria-hidden="true" />}
      {kind === 'unreachable' && <WifiOff className={styles.icon} aria-hidden="true" />}
      {/*
        The live region is the text alone, and it stays mounted while it is empty, so that each text is heard when
        it appears. The buttons are its siblings: the Retry label counts a rate-limit wait down every second, which
        nobody wants read out.
      */}
      <p className={styles.text} aria-live="polite">
        {kind === 'reconnecting' && t('room.connection.reconnecting')}
        {kind === 'restarting' && t('room.connection.restarting')}
        {kind === 'unreachable' && t('room.connection.unreachable')}
      </p>
      {banner?.kind === 'unreachable' && (
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
      )}
    </div>
  );
}
