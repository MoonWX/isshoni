// The room switcher (05 §11.2): a button next to the room's name that opens the list of rooms. It exists only
// while GET /api/v1/rooms says showRoomList: 03 keeps the list hidden until a second room exists, and until then
// admins create that room under Admin → Rooms. Admins also get "Create room" here, a link to /admin/rooms (03 has
// no member endpoint), under the same condition.
//
// Switching is a navigation to the other room's page, whose session then joins it. Joining another room ends this
// page's share (01 §8.4), so while sharing the switcher asks first: "Stop sharing and switch to <room>?" (05 §11.1).
// That question's dialog stays mounted and is closed through `open`, and it opens with the focus on the switcher's
// button: a native <dialog> gives the focus back to where it was when it opened, if it is closed while in the page
// (05 §16.6), and the link that was clicked is gone with the list by then.
import { ChevronsUpDown, Plus } from 'lucide-react';
import { useCallback, useEffect, useRef, useState } from 'react';
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
  /** The room the question is (or was last) about: a switch away from a running share waits to be confirmed. */
  const [asked, setAsked] = useState<Room | null>(null);
  const [asking, setAsking] = useState(false);
  /** The switcher's button, which is also the Popover's. */
  const button = useRef<HTMLButtonElement | null>(null);

  // One callback for the component's life. Popover takes the focus into its panel whenever onOpenChange changes,
  // and the page renders this again on every room.state, and when the list it refetched on opening arrives: a
  // new function each time would pull the focus off the room a keyboard user had reached.
  const onOpenRef = useRef(onOpen);
  useEffect(() => {
    onOpenRef.current = onOpen;
  });
  const onOpenChange = useCallback((open: boolean) => {
    if (open) onOpenRef.current?.();
  }, []);

  if (rooms?.showRoomList !== true) return null;

  return (
    <>
      <Popover
        label={t('room.switcher.label')}
        onOpenChange={onOpenChange}
        trigger={({ ref, ...props }) => (
          <button
            type="button"
            {...props}
            ref={(el) => {
              ref.current = el;
              button.current = el;
            }}
            className={styles.trigger}
            aria-label={t('room.switcher.open')}
          >
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
                          button.current?.focus();
                          setAsked(room);
                          setAsking(true);
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
      {asked !== null && (
        // From the first question on it stays, closed.
        <Dialog
          open={asking}
          size="sm"
          title={t('room.switcher.confirm.title', { room: asked.name })}
          onClose={() => {
            setAsking(false);
          }}
          footer={
            <>
              <Button
                onClick={() => {
                  setAsking(false);
                }}
              >
                {t('common.cancel')}
              </Button>
              <Button
                variant="danger"
                onClick={() => {
                  setAsking(false);
                  void navigate(roomPath(asked.id));
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
