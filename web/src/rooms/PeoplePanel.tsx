// The people panel (05 §11.2): a drawer with everyone in the room, from room.state. Each person has a "Sharing"
// badge while they share, and a person who is reconnecting is dimmed (05 §11.1). A click on a person who shares
// puts their share on the stage. Other people's OS, app version and devices are not shown (01 §8.5 privacy).
//
// The admin badge shows on the user's own row only: room.state says who is in the room, not who is an admin (01
// §8.5), so the page knows it of nobody but its own user (GET /api/v1/me).
//
// The page keeps the panel mounted and closes it with `open`, so that the browser gives the focus back to the
// people count when it closes (05 §16.6). Closed, it lists nobody: the rows are rendered only while it shows.
import { MonitorUp, ShieldCheck } from 'lucide-react';
import { useTranslation } from 'react-i18next';

import {
  ParticipantStatusReconnecting,
  ShareStatusStarting,
  type ParticipantInfo,
  type ShareInfo,
} from '../protocol/types.gen';
import { cx } from '../ui/cx';
import { Sheet } from '../ui/Sheet';
import styles from './PeoplePanel.module.css';

export interface PeoplePanelProps {
  open: boolean;
  onClose: () => void;
  /** room.state.participants. */
  participants: readonly ParticipantInfo[];
  /** room.state.shares. */
  shares: readonly ShareInfo[];
  /** This user's id (welcome.user.id): their row reads "alex (you)". */
  selfUserId: string | null;
  /** This user is an admin (me.user.role). */
  selfIsAdmin: boolean;
  /** A person who shares was clicked: focus that share. The panel closes itself. */
  onWatch: (shareId: string) => void;
}

/** The first character of a name, for the avatar: a whole code point, so an emoji or a kanji stays in one piece. */
function initial(name: string): string {
  const first = name.codePointAt(0);
  return first === undefined ? '' : String.fromCodePoint(first).toUpperCase();
}

/** The share a click on its owner focuses: their newest one that has a tile (not `starting`, 01 §4.4). */
function watchableShare(shares: readonly ShareInfo[], userId: string): ShareInfo | undefined {
  return shares
    .filter((s) => s.userId === userId && s.status !== ShareStatusStarting)
    .reduce<ShareInfo | undefined>((newest, s) => (newest && newest.startedAt >= s.startedAt ? newest : s), undefined);
}

export function PeoplePanel({
  open,
  onClose,
  participants,
  shares,
  selfUserId,
  selfIsAdmin,
  onWatch,
}: PeoplePanelProps) {
  const { t } = useTranslation();
  return (
    <Sheet open={open} onClose={onClose} side="end" title={t('room.people.title', { count: participants.length })}>
      {open && participants.length === 0 && <p className={styles.none}>{t('room.people.none')}</p>}
      {open && participants.length > 0 && (
        <ul className={styles.list}>
          {participants.map((p) => {
            const self = p.userId === selfUserId;
            const sharing = shares.some((s) => s.userId === p.userId);
            const share = watchableShare(shares, p.userId);
            const reconnecting = (p.status as string) === ParticipantStatusReconnecting;
            const row = (
              <>
                <span className={styles.avatar} aria-hidden="true">
                  {initial(p.name)}
                </span>
                <span className={styles.name}>{self ? t('room.people.you', { name: p.name }) : p.name}</span>
                {reconnecting && <span className={styles.status}>{t('room.people.reconnecting')}</span>}
                {self && selfIsAdmin && (
                  <span className={styles.badge}>
                    <ShieldCheck aria-hidden="true" />
                    {t('room.people.admin')}
                  </span>
                )}
                {sharing && (
                  <span className={cx(styles.badge, styles.live)}>
                    <MonitorUp aria-hidden="true" />
                    {t('room.people.sharing')}
                  </span>
                )}
              </>
            );
            return (
              <li key={p.userId} className={cx(styles.person, reconnecting && styles.dim)} data-user-id={p.userId}>
                {share ? (
                  <button
                    type="button"
                    className={cx(styles.row, styles.action)}
                    title={self ? t('room.people.watchOwn') : t('room.people.watch', { name: p.name })}
                    onClick={() => {
                      onWatch(share.id);
                      onClose();
                    }}
                  >
                    {row}
                  </button>
                ) : (
                  <div className={styles.row}>{row}</div>
                )}
              </li>
            );
          })}
        </ul>
      )}
    </Sheet>
  );
}
