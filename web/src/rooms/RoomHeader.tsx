// The room page's header (05 §11.2): the room's name (the page's <h1>), the switcher when there is more than one
// room, the people count ("5 here", which opens the people panel), the share controls the page passes in, and the
// account menu.
import { Users } from 'lucide-react';
import type { ReactNode } from 'react';
import { useTranslation } from 'react-i18next';

import { AppMark } from '../app/AppMark';
import { isAdmin } from '../app/guards';
import type { Me, Rooms } from '../protocol/api.gen';
import { Button } from '../ui/Button';
import { AccountMenu } from './AccountMenu';
import styles from './RoomHeader.module.css';
import { RoomSwitcher } from './RoomSwitcher';

export interface RoomHeaderProps {
  /** The room this page shows (the route's id). */
  roomId: string;
  /** The room's name; undefined until the join or the room list told it. */
  name: string | undefined;
  /** room.state.participants.length; undefined until the first room.state: then there is no count to show. */
  peopleCount: number | undefined;
  /** The people count was clicked. */
  onOpenPeople: () => void;
  /** GET /api/v1/rooms: the switcher shows only when it says showRoomList. */
  rooms: Rooms | undefined;
  /** GET /api/v1/me. */
  me: Me | null | undefined;
  /** This page shares: switching rooms asks first. */
  sharing: boolean;
  /** The switcher's list opened. */
  onOpenRooms?: () => void;
  /** Opens the connection test: the account menu's "Test my connection" (05 §14.2), there only when this is given. */
  onTestConnection?: () => void;
  /** The share controls (the Share button where the platform can share, Stop while sharing). */
  children?: ReactNode;
}

export function RoomHeader({
  roomId,
  name,
  peopleCount,
  onOpenPeople,
  rooms,
  me,
  sharing,
  onOpenRooms,
  onTestConnection,
  children,
}: RoomHeaderProps) {
  const { t } = useTranslation();
  return (
    <header className={styles.header}>
      <div className={styles.lead}>
        <AppMark size={24} className={styles.mark} />
        <h1 className={styles.name}>{name ?? t('room.header.unnamed')}</h1>
        <RoomSwitcher
          rooms={rooms}
          currentId={roomId}
          isAdmin={me != null && isAdmin(me)}
          sharing={sharing}
          onOpen={onOpenRooms}
        />
        {peopleCount !== undefined && (
          <Button
            variant="ghost"
            className={styles.people}
            icon={<Users />}
            aria-haspopup="dialog"
            aria-label={t('room.header.peopleOpen', { count: peopleCount })}
            onClick={onOpenPeople}
          >
            {t('room.header.people', { count: peopleCount })}
          </Button>
        )}
      </div>
      <div className={styles.actions}>
        {children}
        {me != null && <AccountMenu me={me} onTestConnection={onTestConnection} />}
      </div>
    </header>
  );
}
