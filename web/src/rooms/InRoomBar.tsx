// InRoomBar (05 §11.1): the session is app-level, so it goes on while the user is on /account or /admin: friends
// still see them in the room, their share keeps running and the audible share keeps playing. Away from the room
// page this bar says so, "In Lounge · sharing · [Back] [Leave]", with the connection banner above it.
//
// It renders nothing on a room's page (the page shows the session itself), while the session is in no room, and
// in an app that never opened a room: it reads the runtime only if one exists and never makes one, so no page
// starts a signaling connection by showing this bar. The app's layout mounts it once, above the routes' pages; it
// must be inside the router.
import { useTranslation } from 'react-i18next';
import { Link, useLocation } from 'react-router';
import { useStore } from 'zustand';

import { useApp } from '../app/context';
import { Button, buttonClass } from '../ui/Button';
import { ConnectionBanner } from './ConnectionBanner';
import { isRoomPath, roomPath, useLocalShare } from './hooks';
import styles from './InRoomBar.module.css';
import { peekRoomRuntime, type RoomRuntime } from './runtime';

export function InRoomBar() {
  const app = useApp();
  // Read on every navigation: the first room page makes the runtime, and leaving that page is a navigation.
  const { pathname } = useLocation();
  const runtime = peekRoomRuntime(app);
  if (runtime === undefined || isRoomPath(pathname)) return null;
  return <Bar runtime={runtime} />;
}

function Bar({ runtime }: { runtime: RoomRuntime }) {
  const { t } = useTranslation();
  const roomId = useStore(runtime.stores.room, (s) => s.roomId);
  const name = useStore(runtime.stores.room, (s) => s.room?.name);
  const share = useLocalShare(runtime.session);
  if (roomId === null) return null;
  return (
    <section className={styles.bar} aria-label={t('room.bar.label')}>
      <ConnectionBanner />
      <div className={styles.row}>
        <p className={styles.text}>
          <span className={styles.room}>{t('room.bar.in', { room: name ?? t('room.header.unnamed') })}</span>
          {share !== null && <span className={styles.sharing}>{t('room.bar.sharing')}</span>}
        </p>
        <Link to={roomPath(roomId)} className={buttonClass()}>
          {t('room.bar.back')}
        </Link>
        <Button
          variant="ghost"
          onClick={() => {
            // Stops the share, leaves the room on the server and forgets the room (05 §11.1): the bar goes with it.
            void runtime.session.leave();
          }}
        >
          {t('room.bar.leave')}
        </Button>
      </div>
    </section>
  );
}
