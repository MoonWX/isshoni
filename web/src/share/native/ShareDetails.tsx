// The Details disclosure of the native share panel (docs/m2/03 §10.3): the encoder and whether it is hardware; per
// layer width × height @ fps, the target and sent kbit/s and CaptureToSend p50/p95; and the server's view of the
// layers. The data is the share's view (ShareView.encoder, hw, layers, server). While it is open the panel asks for
// the server's stats (Share.WatchServerStats(true), and false when it closes), which fill ShareView.server. It shows
// no source or app names. 01 §12.2's latency run reads the encoder and the full layer's bitrate here.
import { useEffect, useId } from 'react';
import { useTranslation } from 'react-i18next';

import type { ShareView } from '../../platform/desktop/bridge';
import styles from './native.module.css';

export interface ShareDetailsProps {
  view: ShareView;
  /** Share.WatchServerStats: called with true on mount, false on unmount. */
  watchServerStats: (on: boolean) => void;
}

export function ShareDetails({ view, watchServerStats }: ShareDetailsProps) {
  const { t } = useTranslation();
  const titleId = useId();

  useEffect(() => {
    watchServerStats(true);
    return () => {
      watchServerStats(false);
    };
  }, [watchServerStats]);

  const encoder =
    view.encoder === ''
      ? t('desktop.share.details.none')
      : t('desktop.share.details.encoderValue', {
          encoder: view.encoder,
          kind: view.hw ? t('desktop.share.details.hardware') : t('desktop.share.details.software'),
        });

  return (
    <section className={styles.details} aria-labelledby={titleId} data-share-details="">
      <h3 id={titleId} className={styles.legend}>
        {t('desktop.share.details.toggle')}
      </h3>
      <dl className={styles.facts}>
        <dt>{t('desktop.share.details.encoder')}</dt>
        <dd data-detail="encoder">{encoder}</dd>
        <dt>{t('desktop.share.details.layers')}</dt>
        <dd>
          <ul className={styles.names}>
            {view.layers.map((l) => (
              <li key={l.rid} data-detail={`layer-${l.rid}`}>
                <span>
                  {t('desktop.share.details.layer', { rid: l.rid, width: l.width, height: l.height, fps: l.fps })}
                </span>
                <span className={styles.muted}>
                  {t('desktop.share.details.bitrate', { target: l.targetKbps, sent: l.sentKbps })}
                </span>
                <span className={styles.muted}>
                  {t('desktop.share.details.latency', { p50: l.captureToSendMs.p50, p95: l.captureToSendMs.p95 })}
                </span>
              </li>
            ))}
          </ul>
        </dd>
        <dt>{t('desktop.share.details.server')}</dt>
        <dd>
          {view.server === null ? (
            <span className={styles.muted}>{t('desktop.share.details.serverWaiting')}</span>
          ) : (
            <ul className={styles.names}>
              {view.server.layers.map((l) => (
                <li key={`${l.shareId}-${l.kind}-${l.rid ?? ''}`} data-detail={`server-${l.rid ?? l.kind}`}>
                  {t('desktop.share.details.serverLayer', {
                    rid: l.rid ?? l.kind,
                    kbps: Math.round(l.bitrate / 1000),
                    loss: Math.round(l.lossPct * 10) / 10,
                  })}
                </li>
              ))}
            </ul>
          )}
        </dd>
      </dl>
    </section>
  );
}
