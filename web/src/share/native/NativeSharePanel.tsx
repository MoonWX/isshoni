// The share panel in the desktop app (docs/m2/03 §10.3): M1's SharePanel (05 §13.7) for the native share, from
// share/native/ (a lazy chunk the room page loads only when platform.sharing.mode is 'native'). It is a view of the
// page's shareStore for the flow (starting, stopping, a failed share until it is dismissed) and of the DesktopShare's
// view (ShareView) for everything the controller knows:
// - the bar: a red dot, "You're live · <label> · Game · 3 watching" and Stop; paused (amber): "Paused · nothing new
//   leaves your PC…" with Resume; "Reconnecting…"; unreachable: "Can't reach the server's media port" with
//   [Test connection], which opens serverOrigin/r/<room>?conntest=1 in the browser (M1's connection test, §7.6);
//   under the bar the minimized line, the upload or CPU hint, and "Switched to <encoder>" for 5 s;
// - the expander: Optimize for (Share.SetPreset), Share sound (only for a share started with sound: Share.
//   SetAudioEnabled → 05's SetShareSound), who watches (M1, names from room.state) and the Details disclosure, also
//   toggled with Ctrl+Shift+D while the window has focus (ShareDetails.tsx);
// - a failed share: its text and action (engineErrors.ts), e.g. [Share again] after the graphics driver restarted.
//
// M1's "No sound captured" line doesn't apply to native shares (05's silent-stream check does). Later slices add:
// What friends hear, the banners and the mic prompt card (README WA77), and Pause with the hotkeys' tooltips (WA80).
import { ChevronDown, ChevronUp } from 'lucide-react';
import { useCallback, useEffect, useId, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useStore } from 'zustand';

import { useApp } from '../../app/context';
import { errorMessage } from '../../lib/errorText';
import { isDesktopShare } from '../../platform/desktop/DesktopShare';
import type { ActiveShare } from '../../platform/types';
import type { Preset } from '../../protocol/types.gen';
import { Button } from '../../ui/Button';
import { cx } from '../../ui/cx';
import { PRESETS } from '../presets';
import { presetText } from '../presetText';
import { shareStore, type ShareStore, type StartShare } from '../shareStore';
import { linkShareUi } from '../shareUi';
import type { ShareWatcher } from '../watchers';
import styles from './native.module.css';
import { ShareDetails } from './ShareDetails';
import { ShareErrorNote } from './ShareErrorNote';
import { useEncoderNotice, useShareView } from './useShareView';

const NO_WATCHERS: readonly ShareWatcher[] = [];

export interface NativeSharePanelProps {
  /** The session's share (RoomSession.share): the DesktopShare; null while there is none. */
  share: ActiveShare | null;
  /** Stop: the room session's stopShare (05 §11.1). */
  onStop: () => unknown;
  /** Who watches the share: shareWatchers(room.state, share.shareId). */
  watchers?: readonly ShareWatcher[];
  /**
   * Publishes a picked source: the room session's startShare, for [Share again] after a failure (a new pick, whose
   * sheets the page's native Share button shows). Not offered without it.
   */
  onStart?: StartShare;
  /** Default: the page's shareStore. */
  store?: ShareStore;
}

/** Whether a key press is the Details shortcut, Ctrl+Shift+D (docs/m2/03 §10.3). */
function isDetailsKey(e: KeyboardEvent): boolean {
  return e.ctrlKey && e.shiftKey && !e.altKey && !e.metaKey && e.key.toLowerCase() === 'd';
}

export function NativeSharePanel({
  share,
  onStop,
  watchers = NO_WATCHERS,
  onStart,
  store = shareStore,
}: NativeSharePanelProps) {
  const { t } = useTranslation();
  const { ui, prefs, platform } = useApp();
  const phase = useStore(store, (s) => s.phase);
  const preset = useStore(store, (s) => s.preset);
  const soundOn = useStore(store, (s) => s.soundOn);
  const hint = useStore(store, (s) => s.hint);
  const error = useStore(store, (s) => s.error);
  // The session's share, or the one the app publishes that the session doesn't hold (not in this page's room yet,
  // or still starting): the panel shows and stops it all the same.
  const active = share ?? platform.sharing?.current?.() ?? null;
  const view = useShareView(active);
  const encoderNotice = useEncoderNotice(active);
  const [open, setOpen] = useState(false);
  const [detailsOpen, setDetailsOpen] = useState(false);
  const [changingPreset, setChangingPreset] = useState(false);
  const expanderId = useId();
  const presetGroup = useId();

  useEffect(() => {
    linkShareUi(store, ui);
    return store.getState().attachPanel();
  }, [store, ui]);

  const published = phase === 'live' || phase === 'reconnecting';

  // Ctrl+Shift+D toggles the Details disclosure while the window has focus; showing it opens the expander too.
  const detailsShown = useRef(false);
  useEffect(() => {
    detailsShown.current = open && detailsOpen;
  });
  useEffect(() => {
    if (!published) return undefined;
    const onKey = (e: KeyboardEvent): void => {
      if (!isDetailsKey(e)) return;
      e.preventDefault();
      if (detailsShown.current) {
        setDetailsOpen(false);
      } else {
        setOpen(true);
        setDetailsOpen(true);
      }
    };
    window.addEventListener('keydown', onKey);
    return () => {
      window.removeEventListener('keydown', onKey);
    };
  }, [published]);

  const native = isDesktopShare(active) ? active : null;
  const watchServerStats = useCallback(
    (on: boolean) => {
      native?.watchServerStats(on);
    },
    [native],
  );

  if (phase === 'idle' || phase === 'picking' || phase === 'confirming') return null;

  const sharing = platform.sharing;
  const shareAgain =
    onStart !== undefined && sharing !== null
      ? (): void => {
          store.getState().dismiss();
          const picking = sharing.pick({ preset, withAudio: true });
          void store.getState().pick(picking, { preset, start: onStart });
        }
      : undefined;

  if (phase === 'failed') {
    return (
      <section className={cx(styles.panel, styles.failed)} aria-label={t('share.panel.label')}>
        <ShareErrorNote error={error} onShareAgain={shareAgain}>
          <Button
            size="sm"
            onClick={() => {
              store.getState().dismiss();
            }}
          >
            {t('share.panel.dismiss')}
          </Button>
        </ShareErrorNote>
      </section>
    );
  }

  const changePreset = (next: Preset): void => {
    if (active === null || next === preset || changingPreset) return;
    setChangingPreset(true);
    active
      .setPreset(next)
      .then(
        () => {
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

  const viewState = view?.state;
  const paused = viewState === 'paused';
  const unreachable = viewState === 'unreachable';
  const count = watchers.length;
  let status: string;
  if (phase === 'starting') status = t('share.panel.starting');
  else if (phase === 'stopping') status = t('share.panel.stopping');
  else if (unreachable) status = t('desktop.share.panel.unreachable');
  else if (phase === 'reconnecting' || viewState === 'reconnecting') status = t('desktop.share.panel.reconnecting');
  else if (paused) status = t('desktop.share.panel.paused');
  else {
    status = t('desktop.share.panel.live', {
      label: view?.label ?? '',
      preset: presetText(view?.preset ?? preset, t).name,
      watching: t('share.panel.watching', { count }),
    });
  }
  const withAudio = view?.withAudio ?? false;
  const testUrl =
    unreachable && view !== null && platform.serverOrigin !== ''
      ? `${platform.serverOrigin}/r/${encodeURIComponent(view.roomId)}?conntest=1`
      : null;
  const expanded = published && open;

  return (
    <section className={cx(styles.panel, paused && styles.paused)} aria-label={t('share.panel.label')}>
      <div className={styles.bar}>
        <span
          className={cx(styles.dot, phase === 'live' && !paused && styles.dotLive, paused && styles.dotPaused)}
          aria-hidden="true"
        />
        <p className={styles.status} data-share-status={viewState ?? phase}>
          {status}
        </p>
        <div className={styles.actions}>
          {testUrl !== null && (
            <Button
              size="sm"
              onClick={() => {
                platform.openExternal(testUrl);
              }}
            >
              {t('desktop.share.panel.testConnection')}
            </Button>
          )}
          {paused && active !== null && (
            <Button
              size="sm"
              variant="primary"
              onClick={() => {
                active.setPaused(false).catch((err: unknown) => {
                  ui.getState().toast({ kind: 'error', message: errorMessage(err, t) });
                });
              }}
            >
              {t('desktop.share.panel.resume')}
            </Button>
          )}
          {published && (
            <Button
              variant="ghost"
              size="sm"
              aria-expanded={open}
              aria-controls={expanderId}
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
            disabled={active === null && phase === 'starting'}
            aria-label={t('share.panel.stopSharing')}
            onClick={() => {
              if (share !== null || active === null) void onStop();
              else void active.stop();
            }}
          >
            {t('share.panel.stop')}
          </Button>
        </div>
      </div>
      {view?.minimized === true && <p className={styles.hint}>{t('desktop.share.panel.minimized')}</p>}
      {hint !== null && (
        <p className={styles.hint}>
          {hint.kind === 'upload-limited'
            ? t('share.panel.hint.upload', { height: hint.approxHeight })
            : t('share.panel.hint.cpu')}
        </p>
      )}
      {encoderNotice !== null && (
        <p className={styles.note} role="status">
          {t('desktop.share.panel.encoderFallback', { encoder: encoderNotice })}
        </p>
      )}
      {expanded && (
        <div id={expanderId} className={styles.expander}>
          <fieldset className={styles.panelPresets} disabled={active === null || changingPreset}>
            <legend className={styles.legend}>{t('desktop.share.sheet.presetHint')}</legend>
            {PRESETS.map((p) => (
              <label key={p} className={styles.option}>
                <input
                  type="radio"
                  name={presetGroup}
                  value={p}
                  checked={p === (view?.preset ?? preset)}
                  onChange={() => {
                    changePreset(p);
                  }}
                />
                <span>{presetText(p, t).name}</span>
              </label>
            ))}
          </fieldset>
          <div className={styles.column}>
            {withAudio ? (
              <label className={styles.option}>
                <input
                  type="checkbox"
                  role="switch"
                  checked={soundOn}
                  disabled={active === null}
                  onChange={(ev) => {
                    active?.setAudioEnabled(ev.target.checked).catch((err: unknown) => {
                      ui.getState().toast({ kind: 'error', message: errorMessage(err, t) });
                    });
                  }}
                />
                <span>{t('share.panel.sound')}</span>
              </label>
            ) : (
              <p className={styles.muted}>{t('share.panel.soundOff')}</p>
            )}
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
          <div className={styles.column}>
            <Button
              variant="ghost"
              size="sm"
              aria-expanded={detailsOpen}
              aria-keyshortcuts="Control+Shift+D"
              title={t('desktop.share.details.shortcut')}
              onClick={() => {
                setDetailsOpen(!detailsOpen);
              }}
            >
              {t('desktop.share.details.toggle')}
            </Button>
            {detailsOpen && view !== null && <ShareDetails view={view} watchServerStats={watchServerStats} />}
          </div>
        </div>
      )}
    </section>
  );
}
