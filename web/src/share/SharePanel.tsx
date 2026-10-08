// SharePanel (05 §13.7): the sharer's bar at the bottom of the room page while this page shares. A red dot,
// "You're live · Window · Movie · 3 watching", Stop, and an expander with the preset picker, the sound switch, the
// level meter and the list of who is watching. The upload and CPU hints show in the bar itself.
//
// The room page mounts it once:
//   <SharePanel
//     share={share}                                    // RoomSession.share (session.on('share', …))
//     onStop={() => session.stopShare()}               // 05 §11.1
//     watchers={shareWatchers(roomState, share?.shareId)}
//     onTestConnection={…}
//   />
//
// It is a view of shareStore's second half (05 §13.1): nothing while idle or picking; "Starting…" with Stop; the
// live bar; "Reconnecting…" while the pub PC recovers; and a failed share with its error until it is dismissed.
// While a panel is mounted, the Share buttons leave a failed start and the "no sound is shared" note to it.
import { ChevronDown, ChevronUp } from 'lucide-react';
import { useEffect, useId, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useStore } from 'zustand';

import { useApp } from '../app/context';
import { errorMessage } from '../lib/errorText';
import type { ActiveShare } from '../platform/types';
import type { Preset } from '../protocol/types.gen';
import { Button } from '../ui/Button';
import { cx } from '../ui/cx';
import { ShareEndedError } from './BrowserShare';
import { LevelMeter } from './LevelMeter';
import { noAudioNote } from './notes';
import { PRESETS } from './presets';
import { kindText, presetText } from './presetText';
import styles from './SharePanel.module.css';
import { shareStore, type ShareStore } from './shareStore';
import { linkShareUi, shareErrorMessage } from './shareUi';
import type { ShareWatcher } from './watchers';

/** "3 watching" is announced when the count has been the same for this long (05 §13.7). */
export const WATCHERS_ANNOUNCE_MS = 5_000;

const NO_WATCHERS: readonly ShareWatcher[] = [];

export interface SharePanelProps {
  /** This page's share (RoomSession.share); null while it has none. The panel's switches act on it. */
  share: ActiveShare | null;
  /** The Stop button: the room session's stopShare (05 §11.1). */
  onStop: () => unknown;
  /** Who watches the share: shareWatchers(room.state, share.shareId). */
  watchers?: readonly ShareWatcher[];
  /** [Test my connection]: opens the connection test. Without it the button isn't shown. */
  onTestConnection?: () => void;
  /** Default: the page's shareStore. */
  store?: ShareStore;
}

export function SharePanel({
  share,
  onStop,
  watchers = NO_WATCHERS,
  onTestConnection,
  store = shareStore,
}: SharePanelProps) {
  const { t } = useTranslation();
  const { ui, prefs } = useApp();
  const phase = useStore(store, (s) => s.phase);
  const picked = useStore(store, (s) => s.picked);
  const preset = useStore(store, (s) => s.preset);
  const withAudio = useStore(store, (s) => s.withAudio);
  const soundOn = useStore(store, (s) => s.soundOn);
  const hint = useStore(store, (s) => s.hint);
  const unreachable = useStore(store, (s) => s.unreachable);
  const error = useStore(store, (s) => s.error);
  const [open, setOpen] = useState(false);
  const [changingPreset, setChangingPreset] = useState(false);
  const detailsId = useId();
  const presetGroup = useId();

  useEffect(() => {
    linkShareUi(store, ui);
    return store.getState().attachPanel();
  }, [store, ui]);

  // "3 watching", politely, once the count has settled for 5 s; the count the share went live with is not news.
  const published = phase === 'live' || phase === 'reconnecting';
  const count = watchers.length;
  const announced = useRef<number | null>(null);
  useEffect(() => {
    if (!published) {
      announced.current = null;
      return undefined;
    }
    if (announced.current === null) {
      announced.current = count;
      return undefined;
    }
    if (announced.current === count) return undefined;
    const timer = setTimeout(() => {
      announced.current = count;
      ui.getState().announce(t('share.panel.watching', { count }));
    }, WATCHERS_ANNOUNCE_MS);
    return () => {
      clearTimeout(timer);
    };
  }, [published, count, ui, t]);

  if (phase === 'idle' || phase === 'picking' || phase === 'confirming') return null;

  if (phase === 'failed') {
    const mediaTimeout = error instanceof ShareEndedError && error.kind === 'media_timeout';
    return (
      <section className={cx(styles.panel, styles.failed)} aria-label={t('share.panel.label')}>
        <div className={styles.bar}>
          <p className={styles.status} role="alert">
            {shareErrorMessage(error, t)}
          </p>
          <div className={styles.actions}>
            {mediaTimeout && onTestConnection && (
              <Button size="sm" onClick={onTestConnection}>
                {t('share.panel.testConnection')}
              </Button>
            )}
            <Button
              size="sm"
              onClick={() => {
                store.getState().dismiss();
              }}
            >
              {t('share.panel.dismiss')}
            </Button>
          </div>
        </div>
      </section>
    );
  }

  const changePreset = (next: Preset): void => {
    if (share === null || next === preset || changingPreset) return;
    setChangingPreset(true);
    share
      .setPreset(next)
      .then(
        () => {
          // The sharer's last preset is the next share's default (05 §6.1).
          prefs.getState().setPreset(next);
        },
        (err: unknown) => {
          ui.getState().toast({ kind: 'error', message: errorMessage(err, t) });
        },
      )
      .finally(() => {
        setChangingPreset(false);
      });
  };

  let status: string;
  if (phase === 'starting') status = t('share.panel.starting');
  else if (phase === 'stopping') status = t('share.panel.stopping');
  else if (phase === 'reconnecting')
    status = unreachable ? t('share.panel.unreachable') : t('share.panel.reconnecting');
  else {
    status = t('share.panel.live', {
      what: kindText(picked?.kind ?? 'screen', t),
      preset: presetText(preset, t).name,
      watching: t('share.panel.watching', { count }),
    });
  }
  const note = picked !== null ? noAudioNote(picked, t) : null;
  const audioTrack = share?.preview?.getAudioTracks()[0] ?? null;

  return (
    <section className={styles.panel} aria-label={t('share.panel.label')}>
      <div className={styles.bar}>
        <span className={cx(styles.dot, phase === 'live' && styles.dotLive)} aria-hidden="true" />
        <p className={styles.status}>{status}</p>
        <div className={styles.actions}>
          {phase === 'reconnecting' && unreachable && onTestConnection && (
            <Button size="sm" onClick={onTestConnection}>
              {t('share.panel.testConnection')}
            </Button>
          )}
          {published && (
            <Button
              variant="ghost"
              size="sm"
              aria-expanded={open}
              aria-controls={detailsId}
              icon={open ? <ChevronDown /> : <ChevronUp />}
              onClick={() => {
                setOpen(!open);
              }}
            >
              {t('share.panel.details')}
            </Button>
          )}
          <Button
            variant="danger"
            size="sm"
            loading={phase === 'stopping'}
            aria-label={t('share.panel.stopSharing')}
            onClick={() => {
              void onStop();
            }}
          >
            {t('share.panel.stop')}
          </Button>
        </div>
      </div>
      {hint !== null && (
        <p className={styles.hint}>
          {hint.kind === 'upload-limited'
            ? t('share.panel.hint.upload', { height: hint.approxHeight })
            : t('share.panel.hint.cpu')}
        </p>
      )}
      {published && open && (
        <div id={detailsId} className={styles.details}>
          <fieldset className={styles.presets} disabled={share === null || changingPreset}>
            <legend className={styles.legend}>{t('share.preset.legend')}</legend>
            {PRESETS.map((p) => (
              <label key={p} className={styles.preset}>
                <input
                  type="radio"
                  name={presetGroup}
                  value={p}
                  checked={p === preset}
                  onChange={() => {
                    changePreset(p);
                  }}
                />
                <span>{presetText(p, t).name}</span>
              </label>
            ))}
          </fieldset>
          <div className={styles.sound}>
            {withAudio ? (
              <>
                <label className={styles.switch}>
                  <input
                    type="checkbox"
                    role="switch"
                    checked={soundOn}
                    disabled={share === null}
                    onChange={(ev) => {
                      void share?.setAudioEnabled(ev.target.checked);
                    }}
                  />
                  <span>{t('share.panel.sound')}</span>
                </label>
                <LevelMeter track={audioTrack} active={soundOn} />
              </>
            ) : (
              <p className={styles.muted}>{note ?? t('share.panel.soundOff')}</p>
            )}
          </div>
          <div className={styles.watchers}>
            <h3 className={styles.legend}>{t('share.panel.watchers')}</h3>
            {count === 0 ? (
              <p className={styles.muted}>{t('share.panel.nobodyWatching')}</p>
            ) : (
              <ul className={styles.names}>
                {watchers.map((w) => (
                  <li key={w.userId}>{w.name !== '' ? w.name : t('share.panel.someone')}</li>
                ))}
              </ul>
            )}
          </div>
        </div>
      )}
    </section>
  );
}
