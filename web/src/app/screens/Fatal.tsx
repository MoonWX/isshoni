import { Layers, OctagonAlert, ShieldOff, Unplug, type LucideIcon } from 'lucide-react';
import type { TFunction } from 'i18next';
import { useTranslation } from 'react-i18next';

import { Button } from '../../ui/Button';
import type { FatalReason } from '../uiStore';
import { ScreenFrame } from './ScreenFrame';

export interface FatalProps {
  /** Which text to show (05 §7.1): generic "Something went wrong", a disabled account, too many tabs, media. */
  reason?: FatalReason;
  /** The error code, shown as small print. */
  code?: string;
  /** Default: reload the page. */
  onReload?: () => void;
}

// Literal t() calls, so check:i18n sees every key.
function fatalText(reason: FatalReason, t: TFunction): { title: string; body: string; icon: LucideIcon } {
  switch (reason) {
    case 'account_disabled':
      return {
        title: t('common.screens.fatal.accountDisabled.title'),
        body: t('common.screens.fatal.accountDisabled.body'),
        icon: ShieldOff,
      };
    case 'too_many_connections':
      return {
        title: t('common.screens.fatal.tooManyConnections.title'),
        body: t('common.screens.fatal.tooManyConnections.body'),
        icon: Layers,
      };
    case 'media':
      return {
        title: t('common.screens.fatal.media.title'),
        body: t('common.screens.fatal.media.body'),
        icon: Unplug,
      };
    case 'generic':
      return { title: t('common.screens.fatal.title'), body: t('common.screens.fatal.body'), icon: OctagonAlert };
  }
}

/** An unrecoverable error (05 §4, §7.1): the app stops and offers Reload. */
export function Fatal({ reason = 'generic', code, onReload }: FatalProps) {
  const { t } = useTranslation();
  const text = fatalText(reason, t);
  return (
    <ScreenFrame
      icon={text.icon}
      tone="danger"
      title={text.title}
      actions={
        <Button
          variant="primary"
          onClick={() => {
            if (onReload) onReload();
            else globalThis.location.reload();
          }}
        >
          {t('common.reload')}
        </Button>
      }
      footnote={code !== undefined && code !== '' ? t('common.screens.fatal.code', { code }) : undefined}
    >
      <p>{text.body}</p>
    </ScreenFrame>
  );
}
