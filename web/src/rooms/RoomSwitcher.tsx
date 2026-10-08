// The room switcher (05 §11.2): a button next to the room's name that opens the list of rooms. It exists only
// while GET /api/v1/rooms says showRoomList: 03 keeps the list hidden until a second room exists, and until then
// admins create that room under Admin → Rooms. Admins also get "Create room" here, a link to /admin/rooms (03 has
// no member endpoint), under the same condition.
//
// Switching is a navigation to the other room's page, whose session then joins it. Joining another room ends this
// page's share (01 §8.4), so while sharing the switcher asks first: "Stop sharing and switch to <room>?" (05 §11.1).
import { ChevronsUpDown, Plus } from 'lucide-react';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, useNavigate } from 'react-router';

import type { Room, Rooms } from '../protocol/api.gen';
import { Button } from '../ui/Button';
import { Dialog } from '../ui/Dialog';
import { Popover } from '../ui/Popover';
import { roomPath } from './hooks';
import styles from './RoomSwitcher.module.css';

export interface RoomSwitcherProps {
  /** GET /api/v1/rooms; undefined while it loads. */
  rooms: Rooms | undefined;
  /** The room this page shows. */
  currentId: string;
  /** Admins get "Create room". */
  isAdmin: boolean;
  /** This page shares: switching asks first. */
  sharing: boolean;
  /** The list opened. Its live counts are as old as the last fetch, so the page refetches. */
  onOpen?: () => void;
}

export function RoomSwitcher({ rooms, currentId, isAdmin, sharing, onOpen }: RoomSwitcherProps) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  /** The room a switch away from a running share waits to be confirmed for. */
  const [asking, setAsking] = useState<Room | null>(null);

  if (rooms?.showRoomList !== true) return null;

  return (
    <>
      <Popover
        label={t('room.switcher.label')}
        onOpenChange={(open) => {
          if (open) onOpen?.();
        }}
        trigger={(props) => (
          <button type="button" {...props} className={styles.trigger} aria-label={t('room.switcher.open')}>
            <ChevronsUpDown aria-hidden="true" />
          </button>
        )}
      >
        {(close) => (
          <>
            <ul className={styles.list}>
              {rooms.rooms.map((room) => {
                const counts = (
                  <>
                    <span className={styles.name}>{room.name}</span>
                    <span className={styles.counts}>
                      {t('room.switcher.people', { count: room.live.participants })}
                      {room.live.shares > 0 && (
                        <span className={styles.live}>{t('room.switcher.live', { count: room.live.shares })}</span>
                      )}
                    </span>
                  </>
                );
                return (
                  <li key={room.id}>
                    {room.id === currentId ? (
                      // The room the page is in: marked, and nothing to switch to.
                      <div className={styles.room} aria-current="true">
                        {counts}
                      </div>
                    ) : (
                      <Link
                        to={roomPath(room.id)}
                        className={styles.room}
                        onClick={(e) => {
                          close();
                          if (!sharing) return;
                          e.preventDefault();
                          setAsking(room);
                        }}
                      >
                        {counts}
                      </Link>
                    )}
                  </li>
                );
              })}
            </ul>
            {isAdmin && (
              <Link to="/admin/rooms" className={styles.create} onClick={close}>
                <Plus aria-hidden="true" />
                {t('room.switcher.create')}
              </Link>
            )}
          </>
        )}
      </Popover>
      {asking !== null && (
        <Dialog
          open
          size="sm"
          title={t('room.switcher.confirm.title', { room: asking.name })}
          onClose={() => {
            setAsking(null);
          }}
          footer={
            <>
              <Button
                onClick={() => {
                  setAsking(null);
                }}
              >
                {t('common.cancel')}
              </Button>
              <Button
                variant="danger"
                onClick={() => {
                  setAsking(null);
                  void navigate(roomPath(asking.id));
                }}
              >
                {t('room.switcher.confirm.action')}
              </Button>
            </>
          }
        >
          <p>{t('room.switcher.confirm.body')}</p>
        </Dialog>
      )}
    </>
  );
}
