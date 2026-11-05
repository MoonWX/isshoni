import { RefreshCw } from 'lucide-react';
import { lazy, Suspense } from 'react';
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

/** The desktop app's variant (docs/m2/03 §16.3), loaded on demand: the browser never fetches it. */
const DesktopVersionMismatch = lazy(() =>
  import('../../desktop/shell').then((m) => ({ default: m.DesktopVersionMismatch })),
);

/**
 * This page and the server don't speak a common protocol version (05 §16.4). The connection wiring
 * (rooms/connection.ts) shows it for `stopped` with protocol_unsupported or client_outdated, at once and without a
 * reload of its own; a second time for the same server version means that the Reload didn't help (`stillStale`).
 * In the desktop app (M2: actions with updateApp or openInBrowser, docs/m2/03 §16.3) it is desktop/VersionScreen.tsx
 * instead: Update the app and Open in browser (the current room), and Try again; the browser's screen is unchanged.
 * A welcome from a server of another version is not this screen: the page reloads once, or the update pill waits
 * while it shares. e2e/version.spec.ts checks both through a routed signaling socket.
 */
export function VersionMismatch({
  serverVersion,
  serverOlder = false,
  stillStale = false,
  actions,
}: VersionMismatchProps) {
  const { t } = useTranslation();
  if (actions.updateApp !== undefined || actions.openInBrowser !== undefined) {
    return (
      <Suspense fallback={null}>
        <DesktopVersionMismatch
          {...(serverVersion !== undefined ? { serverVersion } : {})}
          serverOlder={serverOlder}
          actions={actions}
        />
      </Suspense>
    );
  }
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
