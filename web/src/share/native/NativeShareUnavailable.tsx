// Where the Share button would be when this PC can't share (docs/m2/03 §10.5's probe rows): Share.Capabilities()
// answered canShare false with the media code as reason (unsupported_os, no_encoder, …). Viewing works.
// - NativeShareUnavailable: the note in the empty state's place of [Share your screen];
// - NativeShareDisabled: the header's Share, disabled "with" the same text (§10.5): its description and its tooltip,
//   so a room that already has shares says why too.
//
// ShareCaps carries only the media code, so the probe's reason rows (no_media_foundation, blocked_by_policy) and
// NVENC's quiet driver_too_old note can't be told yet: that needs a per-encoder status and the reason token in
// ShareCaps from Go (internal/desktop/bridge), handed over with README WA65's review notes.
import type { TFunction } from 'i18next';
import { MonitorUp } from 'lucide-react';
import { useId, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';

import { BridgeError } from '../../platform/desktop/errors';
import { Button } from '../../ui/Button';
import { VisuallyHidden } from '../../ui/VisuallyHidden';
import { shareErrorText } from './engineErrors';
import styles from './native.module.css';

export interface NativeShareUnavailableProps {
  /** ShareCaps.reason: a media code; '' or undefined when the probe gave none. */
  reason?: string | undefined;
}

/** "Sharing isn't available on this PC. <the reason's text>". */
function unavailableText(reason: string | undefined, t: TFunction): string {
  const why = shareErrorText(
    new BridgeError(`engine.${reason !== undefined && reason !== '' ? reason : 'internal'}`),
    t,
  );
  return t('desktop.share.unavailable', { reason: why });
}

export function NativeShareUnavailable({ reason }: NativeShareUnavailableProps) {
  const { t } = useTranslation();
  return <p className={styles.muted}>{unavailableText(reason, t)}</p>;
}

export interface NativeShareDisabledProps extends NativeShareUnavailableProps {
  /** The button's text, as the Share button's. */
  label: ReactNode;
}

export function NativeShareDisabled({ reason, label }: NativeShareDisabledProps) {
  const { t } = useTranslation();
  const id = useId();
  const text = unavailableText(reason, t);
  return (
    // The tooltip is the wrapper's: a disabled button gets no pointer events of its own.
    <span className={styles.disabledShare} title={text}>
      <Button variant="primary" icon={<MonitorUp />} disabled aria-describedby={id}>
        {label}
      </Button>
      <VisuallyHidden id={id}>{text}</VisuallyHidden>
    </span>
  );
}
