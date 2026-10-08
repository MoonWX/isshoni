// UpdatePill (05 §16.2, §16.4): "Update ready · Reload", a small pill at the top of the screen while a new build
// waits. The shell (App.tsx) mounts it once, outside the router.
//
// Where "ready" comes from: uiStore.updateReady. This component sets it when platform.pwa reports an update (a new
// service worker waits, or /version.json changed: platform/browser/pwa.ts); other parts may set it too (§16.4: a
// stale build seen at `welcome` while the user is sharing, when the page must not reload by itself).
//
// Reload calls platform.pwa.applyUpdate(): the waiting worker takes over and the page reloads. On a platform without
// a PWA provider (the desktop app, M2) it is the platform's reload action.
//
// While the user shares their screen a reload would end the share, so the pill only says that an update waits and
// offers no button (§16.2 "not while sharing"); it gets its button back when the share ends. "Sharing" is
// uiStore.sharing, which share/ keeps equal to its share state (share/shareUi.ts).
import { RefreshCw } from 'lucide-react';
import { useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';

import { Button } from '../ui/Button';
import { cx } from '../ui/cx';
import { useApp, useUi } from './context';
import styles from './UpdatePill.module.css';

export function UpdatePill() {
  const { t } = useTranslation();
  const { platform, ui } = useApp();
  const ready = useUi((s) => s.updateReady);
  const sharing = useUi((s) => s.sharing);
  const [reloading, setReloading] = useState(false);

  useEffect(
    () =>
      platform.pwa?.onUpdateReady(() => {
        ui.getState().setUpdateReady(true);
      }),
    [platform, ui],
  );

  // Screen readers hear it through the Announcer's live region: a region inserted together with its text is
  // announced unreliably. Once per page, also under StrictMode's second effect run.
  const announced = useRef(false);
  useEffect(() => {
    if (!ready || announced.current) return;
    announced.current = true;
    ui.getState().announce(t('common.update.announce'));
  }, [ready, ui, t]);

  if (!ready) return null;

  const reload = (): void => {
    setReloading(true);
    if (platform.pwa) platform.pwa.applyUpdate();
    else platform.versionActions().reload?.();
  };

  return (
    <section className={styles.region} aria-label={t('common.update.label')}>
      <div className={cx(styles.pill, sharing && styles.passive)}>
        <RefreshCw className={styles.icon} aria-hidden="true" />
        <p className={styles.text}>{sharing ? t('common.update.afterSharing') : t('common.update.ready')}</p>
        {!sharing && (
          <Button variant="primary" size="sm" className={styles.reload} loading={reloading} onClick={reload}>
            {t('common.reload')}
          </Button>
        )}
      </div>
    </section>
  );
}
