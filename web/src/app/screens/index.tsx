// App-level screens (05 §4): each replaces the whole app.
import type { Platform } from '../../platform/types';
import type { AppScreen } from '../uiStore';
import { Fatal } from './Fatal';
import { NeedsHttps } from './NeedsHttps';
import { NotSetUp } from './NotSetUp';
import { Offline } from './Offline';
import { Unsupported } from './Unsupported';
import { VersionMismatch } from './VersionMismatch';

export { Fatal, NeedsHttps, NotSetUp, Offline, Unsupported, VersionMismatch };

export interface AppScreenViewProps {
  screen: AppScreen;
  /** For VersionMismatch's actions; null before boot has a platform. */
  platform: Platform | null;
  /** For Offline: try again now. */
  onRetry?: () => void;
  retrying?: boolean;
}

/** Renders an AppScreen value (uiStore.screen, or boot's own). */
export function AppScreenView({ screen, platform, onRetry, retrying = false }: AppScreenViewProps) {
  switch (screen.kind) {
    case 'offline':
      return (
        <Offline
          onRetry={
            onRetry ??
            (() => {
              globalThis.location.reload();
            })
          }
          retrying={retrying}
        />
      );
    case 'needsHttps':
      return <NeedsHttps />;
    case 'notSetUp':
      return <NotSetUp />;
    case 'unsupported':
      return <Unsupported />;
    case 'fatal':
      return <Fatal reason={screen.reason} code={screen.code} onReload={platform?.versionActions().reload} />;
    case 'versionMismatch':
      return (
        <VersionMismatch
          serverVersion={screen.serverVersion}
          serverOlder={screen.serverOlder}
          stillStale={screen.stillStale}
          actions={
            platform?.versionActions() ?? {
              reload: () => {
                globalThis.location.reload();
              },
            }
          }
        />
      );
  }
}
