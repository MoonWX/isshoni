// "Open in the isshoni app" (docs/m2/03 §12.3): in the browser, when the user probably has the desktop app, an entry
// that opens the room in it. "Probably" is either of:
// - user.connections (the handoff store, handoff.ts) lists a desktop connection that is online;
// - GET /api/v1/me/devices lists a device with clientKind desktop and the same os as this browser's client.os.
// It opens appLinkFor(serverOrigin, roomId) (lib/appLink.ts, isshoni://<host>/r/<roomId>) through
// platform.openExternal; 3 s later, unless the app took over (the tab was handed off), a quiet line says "Nothing
// happened? [Get the desktop app]" with the server's /download?room=<roomId> page. When appLinkFor gives "" (an
// http:// server, or a host its rules refuse), there is no entry at all. The app then joins the room and sends
// playback.stop (03 §12.2), which hands this tab off.
//
// Nothing in the desktop app, whose page is the app.
import { useQuery } from '@tanstack/react-query';
import { useEffect, useState } from 'react';
import { Trans, useTranslation } from 'react-i18next';
import { Link } from 'react-router';
import { useStore } from 'zustand';

import { useApp } from '../app/context';
import { appLinkFor, validRoomId } from '../lib/appLink';
import type { DevicesResponse } from '../protocol/api.gen';
import { queryKeys } from '../protocol/queryKeys';
import { api } from '../protocol/rest';
import { ClientKindDesktop, type OwnConnection } from '../protocol/types.gen';
import { Button } from '../ui/Button';
import { onlineDesktops } from './handoff';
import styles from './OpenInApp.module.css';
import type { RoomRuntime } from './runtime';

/** After a click, "Nothing happened?" waits this long for the app (03 §12.3). */
export const NOTHING_HAPPENED_AFTER_MS = 3_000;

/** The server's download page for a room: its "Open isshoni" link opens that room (06 §7.5). */
export function downloadPath(roomId: string): string {
  return validRoomId(roomId) ? `/download?room=${roomId}` : '/download';
}

/**
 * Whether the user probably has the desktop app (03 §12.3): an online desktop connection, or a linked desktop device
 * on this browser's os.
 */
export function appLikely(
  connections: readonly OwnConnection[] | null,
  devices: DevicesResponse | undefined,
  os: string,
): boolean {
  if (onlineDesktops(connections).length > 0) return true;
  return devices?.devices.some((d) => d.clientKind === ClientKindDesktop && d.os === os) === true;
}

export interface OpenInAppProps {
  runtime: Pick<RoomRuntime, 'handoff' | 'viewer'>;
  roomId: string;
}

export function OpenInApp({ runtime, roomId }: OpenInAppProps) {
  const { platform } = useApp();
  // The desktop's page is the app; a server without https has no link.
  const link = platform.desktop == null ? appLinkFor(platform.serverOrigin, roomId) : '';
  if (link === '') return null;
  // Another room is a new question: the entry starts again.
  return <Entry key={roomId} runtime={runtime} roomId={roomId} link={link} />;
}

function Entry({ runtime, roomId, link }: OpenInAppProps & { link: string }) {
  const { t } = useTranslation();
  const { platform } = useApp();
  const connections = useStore(runtime.handoff.store, (s) => s.connections);
  const handedOff = useStore(runtime.viewer.store, (s) => s.handedOff);
  const online = onlineDesktops(connections).length > 0;
  // The device list only when the connections don't already say so. Shared with the Devices page's cache.
  const devices = useQuery({
    queryKey: queryKeys.meDevices,
    queryFn: ({ signal }) => api<DevicesResponse>('GET', '/api/v1/me/devices', undefined, { signal }),
    enabled: !online,
    staleTime: 5 * 60_000,
    retry: false,
  });
  /** Date.now() of the last click; null before one. */
  const [openedAt, setOpenedAt] = useState<number | null>(null);
  /** The click that NOTHING_HAPPENED_AFTER_MS has passed for. */
  const [lateFor, setLateFor] = useState<number | null>(null);
  const late = openedAt !== null && lateFor === openedAt;

  useEffect(() => {
    if (openedAt === null) return undefined;
    const timer = setTimeout(() => {
      setLateFor(openedAt);
    }, NOTHING_HAPPENED_AFTER_MS);
    return () => {
      clearTimeout(timer);
    };
  }, [openedAt]);

  if (!appLikely(connections, devices.data, platform.client.os)) return null;
  return (
    <span className={styles.entry}>
      <Button
        variant="ghost"
        onClick={() => {
          platform.openExternal(link);
          setOpenedAt(Date.now());
        }}
      >
        {t('room.openInApp.open')}
      </Button>
      {late && !handedOff && (
        <span className={styles.nothing} role="status">
          <Trans i18nKey="room.openInApp.nothing" components={{ a: <Link to={downloadPath(roomId)} /> }} />
        </span>
      )}
    </span>
  );
}
