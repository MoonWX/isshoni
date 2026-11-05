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
//   [Share again] and [Pick again] start a new pick here; the page's native Share button shows its sheets, and a pick
//   that fails (self_window, permission_denied, …) before any share is said as a toast, since the panel goes with it.
//
// - What friends hear (README WA77): 05's mic prompt cards at the top of the panel (Keep out or Include, ✕ and Esc
//   dismiss) and its banners under the bar, always shown; in the expander the three groups with each app's level bar
//   and the overall meter (Audio.WatchLevels while the expander is open), Keep out or Include on the rows that have a
//   toggle, the ducking tip with "Open sound settings", and the volume note under Share sound.
//
// M1's "No sound captured" line doesn't apply to native shares (05's silent-stream check does): while sound is on
// and no heard app plays, the panel says "Nothing is playing right now".
//
// Share safety (README WA80, 03 §10.3, §11.2, §11.3, §11.6): Pause next to Stop while live (Share.SetPaused, the same
// path as the hotkey, the tray and the pill), the hotkeys named in the buttons' tooltips ("Pause (Ctrl+Alt+Shift+F9)"),
// "Pause hotkey unavailable: another app uses …" with a link to Settings → Sharing for a combination another app
// has, and Pause disabled with "Pause needs a newer server" once the server answered feature_disabled. The close-to-
// tray notice (WindowNotice) is the desktop shell's, above every page (app/App.tsx, desktop/shell.ts).
import { ChevronDown, ChevronUp } from 'lucide-react';
import { useCallback, useEffect, useId, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router';
import { useStore } from 'zustand';

import { useApp } from '../../app/context';
import { errorMessage } from '../../lib/errorText';
import { BridgeError } from '../../platform/desktop/errors';
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
import { shareFailureText } from './engineErrors';
import {
  AppGroups,
  AudioBanners,
  AudioTips,
  MicPromptCards,
  MixMeter,
  nothingPlaying,
  VolumeNote,
} from './FriendsHear';
import styles from './native.module.css';
import { ShareDetails } from './ShareDetails';
import { ShareErrorNote } from './ShareErrorNote';
import { useMicPromptHost } from './MicPromptCorner';
import { useAudioBridge, useFriendsHear, useLevels } from './useAudio';
import { usable, useHotkeys } from './useHotkeys';
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
  const audioBridge = useAudioBridge();
  const showsShare = phase !== 'idle' && phase !== 'picking' && phase !== 'confirming' && phase !== 'failed';
  const fh = useFriendsHear(showsShare ? audioBridge : null);
  // The panel shows the mic prompts while it shows the share: the corner card (MicPromptCorner) steps aside.
  useMicPromptHost(showsShare && audioBridge !== null);

  useEffect(() => {
    linkShareUi(store, ui);
    return store.getState().attachPanel();
  }, [store, ui]);

  const published = phase === 'live' || phase === 'reconnecting';
  const levels = useLevels(audioBridge, published && open);
  // The hotkeys are registered at live: ask again when the share's state changes.
  const hotkeys = useHotkeys(showsShare ? audioBridge : null, view?.state ?? null);
  // The server answered feature_disabled: no share.pause there (03 §11.3).
  const [pauseUnavailable, setPauseUnavailable] = useState(false);
  const navigate = useNavigate();

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
          void store
            .getState()
            .pick(picking, { preset, start: onStart })
            .then((outcome) => {
              // A failed start stays in this panel (phase failed). A failed pick left the flow idle, so the panel is
              // gone: say it as a toast.
              if (outcome.step !== 'failed' || store.getState().phase === 'failed') return;
              const message = shareFailureText(outcome.error, t);
              if (message !== '') ui.getState().toast({ kind: 'error', message });
            });
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
  // Pause while the share runs (live, reconnecting or unreachable); Resume replaces it while paused.
  const canPause = !paused && (viewState === 'live' || viewState === 'reconnecting' || viewState === 'unreachable');
  // "Pause (Ctrl+Alt+Shift+F9)": the hotkeys are named in the buttons' tooltips while they work (03 §10.3).
  const withHotkey = (action: string, combo: string | null): string =>
    combo === null ? action : t('desktop.share.panel.withHotkey', { action, hotkey: combo });
  const pauseTitle = (action: string): string => withHotkey(action, usable(hotkeys.pause) ? hotkeys.pause.combo : null);
  const stopTitle = withHotkey(t('desktop.share.panel.stop'), usable(hotkeys.stop) ? hotkeys.stop.combo : null);
  // A combination another app has (03 §11.2): said under the bar, with the way to change it.
  const taken = [
    hotkeys.pause?.state === 'taken'
      ? t('desktop.share.panel.hotkeyTakenPause', { hotkey: hotkeys.pause.combo })
      : null,
    hotkeys.stop?.state === 'taken' ? t('desktop.share.panel.hotkeyTakenStop', { hotkey: hotkeys.stop.combo }) : null,
  ].filter((s): s is string => s !== null);
  const takenNotes =
    published && taken.length > 0 ? (
      <p className={styles.hotkeyNote} data-hotkey-note="">
        <span>{taken.join(' · ')}</span>
        <Button size="sm" variant="ghost" onClick={() => void navigate('/desktop/settings#sharing')}>
          {t('desktop.share.panel.hotkeySettings')}
        </Button>
      </p>
    ) : null;

  return (
    <section className={cx(styles.panel, paused && styles.paused)} aria-label={t('share.panel.label')}>
      {fh !== null && <MicPromptCards fh={fh} bridge={audioBridge} />}
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
              title={pauseTitle(t('desktop.share.panel.resume'))}
              onClick={() => {
                active.setPaused(false).catch((err: unknown) => {
                  ui.getState().toast({ kind: 'error', message: errorMessage(err, t) });
                });
              }}
            >
              {t('desktop.share.panel.resume')}
            </Button>
          )}
          {canPause && active !== null && (
            <Button
              size="sm"
              disabled={pauseUnavailable}
              title={
                pauseUnavailable
                  ? t('desktop.share.panel.pauseNeedsNewerServer')
                  : pauseTitle(t('desktop.share.panel.pause'))
              }
              onClick={() => {
                active.setPaused(true).catch((err: unknown) => {
                  if (err instanceof BridgeError && err.code === 'feature_disabled') {
                    setPauseUnavailable(true);
                    return;
                  }
                  ui.getState().toast({ kind: 'error', message: errorMessage(err, t) });
                });
              }}
            >
              {t('desktop.share.panel.pause')}
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
            title={stopTitle}
            onClick={() => {
              if (share !== null || active === null) void onStop();
              else void active.stop();
            }}
          >
            {t('share.panel.stop')}
          </Button>
        </div>
      </div>
      {takenNotes}
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
      {fh !== null && <AudioBanners fh={fh} />}
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
            {withAudio && <VolumeNote />}
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
          {withAudio && fh !== null && (
            <div className={styles.column} data-section="friends-hear">
              <h3 className={styles.legend}>{t('desktop.audio.title')}</h3>
              <MixMeter levels={levels} />
              {nothingPlaying(fh, soundOn) && <p className={styles.muted}>{t('desktop.audio.nothingPlaying')}</p>}
              <AppGroups fh={fh} bridge={audioBridge} levels={levels} />
              <AudioTips
                fh={fh}
                bridge={audioBridge}
                openSoundSettings={() => {
                  audioBridge?.app.openOSSettings('sound-communications').catch((err: unknown) => {
                    ui.getState().toast({ kind: 'error', message: errorMessage(err, t) });
                  });
                }}
              />
            </div>
          )}
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
