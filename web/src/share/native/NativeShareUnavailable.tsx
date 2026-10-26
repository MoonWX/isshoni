// Where the Share button would be when this PC can't share (docs/m2/03 §10.5's probe rows): Share.Capabilities()
// answered canShare false with the media code as reason (unsupported_os, no_encoder, …). Viewing works.
import { useTranslation } from 'react-i18next';

import { BridgeError } from '../../platform/desktop/errors';
import { shareErrorText } from './engineErrors';
import styles from './native.module.css';

export interface NativeShareUnavailableProps {
  /** ShareCaps.reason: a media code; '' or undefined when the probe gave none. */
  reason?: string | undefined;
}

export function NativeShareUnavailable({ reason }: NativeShareUnavailableProps) {
  const { t } = useTranslation();
  const why = shareErrorText(
    new BridgeError(`engine.${reason !== undefined && reason !== '' ? reason : 'internal'}`),
    t,
  );
  return <p className={styles.muted}>{t('desktop.share.unavailable', { reason: why })}</p>;
}
