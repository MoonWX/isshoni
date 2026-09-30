import { RefreshCw } from 'lucide-react';
import { useTranslation } from 'react-i18next';

import type { VersionActions } from '../../platform/types';
import { Button } from '../../ui/Button';
import { ScreenFrame } from './ScreenFrame';

export interface VersionMismatchProps {
  /** The server's version, when known (welcome.serverVersion or the protocol_unsupported params). */
  serverVersion?: string;
  /** The server is older than this page: only the admin can fix it (05 §16.4). */
  serverOlder?: boolean;
  /** A reload for this server version already happened and didn't help. */
  stillStale?: boolean;
  /** From platform.versionActions(): the browser reloads; the desktop app (M2) updates or opens a browser. */
  actions: VersionActions;
}

/**
 * This page and the server don't speak a common protocol version (05 §16.4). S93 (05 W13) wires the triggers; the
 * connection wiring shows it for `stopped` with protocol_unsupported.
 */
export function VersionMismatch({
  serverVersion,
  serverOlder = false,
  stillStale = false,
  actions,
}: VersionMismatchProps) {
  const { t } = useTranslation();
  const body = serverOlder
    ? t('common.screens.versionMismatch.serverOlder')
    : stillStale
      ? t('common.screens.versionMismatch.stillStale')
      : t('common.screens.versionMismatch.body');
  const reload = actions.reload;
  return (
    <ScreenFrame
      icon={RefreshCw}
      tone="warning"
      title={t('common.screens.versionMismatch.title')}
      actions={
        reload && !serverOlder ? (
          <Button variant="primary" onClick={reload}>
            {t('common.reload')}
          </Button>
        ) : undefined
      }
      footnote={
        serverVersion !== undefined || stillStale
          ? t('common.screens.versionMismatch.versions', {
              client: __ISSHONI_VERSION__,
              server: serverVersion ?? '?',
            })
          : undefined
      }
    >
      <p>{body}</p>
    </ScreenFrame>
  );
}
