// "What friends hear" in the desktop app (docs/m2/03 §10.2, §10.3; 05 §14.5–§14.8): the parts the share sheet, the
// first-share sheet and the share panel show of 05's FriendsHear.
// - AppGroups: the three groups, "Friends will hear:" (heard), "Friends won't hear:" (keptOut, with their reasons)
//   and "Not captured:" (notCaptured), or "everything on this PC" when the engine is `endpoint`. Each row has its
//   reason and warning by key, its level bar when levels are given (−60 to 0 dBFS of `apps[appId].peakDb`), and Keep
//   out or Include only when `control === 'toggle'` (Audio.SetAppDecision); a locked built-in shows its reason only,
//   or desktop.audio.warning.sharedVoiceApp when it is the shared app.
// - FriendsHearSummary: the share sheet's line "Friends will hear 2 apps · Discord kept out · [Details]"; Details
//   shows the groups.
// - AudioBanners: FriendsHear.banners, always shown and never dismissable (PLAN "Transparency").
// - MicPromptCards: the mic prompts, by Prompt.kind, with Audio.Decide on the buttons and Audio.DismissPrompt on ✕
//   and Esc (never decide(id, 'auto')): the app stays kept out and isn't asked again this run (05 §14.6).
// - AudioTips: 05's ducking tip with "Open sound settings" (App.OpenOSSettings('sound-communications')) and its
//   dismiss (Audio.DismissTip).
// - VolumeNote: "Muting your PC doesn't mute your stream…" under Share sound (S1: loopback ignores the master volume).
// - NothingPlaying: the panel's neutral line while sound is on and no heard row plays; M1's "No sound captured" rule
//   doesn't apply to native shares (05 §8.5's silent-stream check and its capture-trouble banner do).
import { X } from 'lucide-react';
import { useId, useState, type KeyboardEvent } from 'react';
import { useTranslation } from 'react-i18next';

import { useApp } from '../../app/context';
import { errorMessage } from '../../lib/errorText';
import type { AppRow, Decision, FriendsHear, Levels, NativeBridge } from '../../platform/desktop/bridge';
import { bridgeErrorCode } from '../../platform/desktop/errors';
import { Button } from '../../ui/Button';
import { cx } from '../../ui/cx';
import styles from './audio.module.css';
import { bannerText, promptTexts, reasonText, warningText } from './audioText';

/** The level bar's range (docs/m2/03 §10.3). */
export const METER_MIN_DB = -60;
export const METER_MAX_DB = 0;

function meterValue(db: number | undefined): number {
  if (db === undefined || !Number.isFinite(db)) return METER_MIN_DB;
  return Math.min(METER_MAX_DB, Math.max(METER_MIN_DB, db));
}

/** Toasts a failed bridge call with its text. */
function useFailure(): (err: unknown) => void {
  const { t } = useTranslation();
  const { ui } = useApp();
  return (err) => {
    ui.getState().toast({ kind: 'error', message: errorMessage(err, t) });
  };
}

interface RowProps {
  row: AppRow;
  /** Which of the row's decisions its button sets: keep-out for a heard row, include for a kept-out one. */
  toggle: Decision | null;
  bridge: NativeBridge | null;
  levels: Levels | null;
}

function Row({ row, toggle, bridge, levels }: RowProps) {
  const { t } = useTranslation();
  const fail = useFailure();
  const [busy, setBusy] = useState(false);
  const reason = reasonText(row, t);
  const warning = warningText(row, t);
  const decide = (d: Decision): void => {
    if (bridge === null) return;
    setBusy(true);
    bridge.audio
      .setAppDecision(row.id, d)
      .catch(fail)
      .finally(() => {
        setBusy(false);
      });
  };
  return (
    <li className={styles.row} data-app-id={row.id}>
      <div className={styles.rowMain}>
        <span className={styles.name}>{row.name}</span>
        {reason !== '' && <span className={styles.reason}>{reason}</span>}
        {levels !== null && (
          <meter
            className={styles.meter}
            min={METER_MIN_DB}
            max={METER_MAX_DB}
            value={meterValue(levels.apps[row.id]?.peakDb)}
            aria-label={t('desktop.audio.row.level', { app: row.name })}
          />
        )}
        {row.control === 'toggle' && toggle !== null && bridge !== null && (
          <Button
            size="sm"
            loading={busy}
            onClick={() => {
              decide(toggle);
            }}
          >
            {toggle === 'keep-out' ? t('desktop.audio.row.keepOut') : t('desktop.audio.row.include')}
          </Button>
        )}
      </div>
      {warning !== '' && <p className={styles.warning}>{warning}</p>}
    </li>
  );
}

export interface AppGroupsProps {
  fh: FriendsHear;
  bridge: NativeBridge | null;
  /** The level sample to draw the rows' bars from; null for no bars (the sheets). */
  levels?: Levels | null;
}

/** The three groups of FriendsHear, or "everything on this PC" for the endpoint engine. */
export function AppGroups({ fh, bridge, levels = null }: AppGroupsProps) {
  const { t } = useTranslation();
  const heardId = useId();
  const keptId = useId();
  const notId = useId();
  if (fh.engine === 'endpoint') {
    return <p className={styles.everything}>{t('desktop.audio.sheet.everything')}</p>;
  }
  const groups: { id: string; title: string; rows: AppRow[]; toggle: Decision }[] = [
    { id: heardId, title: t('desktop.audio.group.heard'), rows: fh.heard, toggle: 'keep-out' },
    { id: keptId, title: t('desktop.audio.group.keptOut'), rows: fh.keptOut, toggle: 'include' },
    { id: notId, title: t('desktop.audio.group.notCaptured'), rows: fh.notCaptured, toggle: 'keep-out' },
  ];
  return (
    <div className={styles.groups}>
      {groups.map((g) =>
        g.rows.length === 0 ? null : (
          <section key={g.id} aria-labelledby={g.id} className={styles.group}>
            <h4 id={g.id} className={styles.groupTitle}>
              {g.title}
            </h4>
            <ul className={styles.rows}>
              {g.rows.map((row) => (
                <Row key={row.id} row={row} toggle={g.toggle} bridge={bridge} levels={levels} />
              ))}
            </ul>
          </section>
        ),
      )}
    </div>
  );
}

/** The share sheet's one line: who is heard, who is kept out, and [Details] with the groups. */
export function FriendsHearSummary({ fh, bridge }: { fh: FriendsHear; bridge: NativeBridge | null }) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const detailsId = useId();
  const parts: string[] = [];
  if (fh.engine === 'endpoint') {
    parts.push(t('desktop.audio.sheet.everything'));
  } else {
    parts.push(
      fh.heard.length === 0
        ? t('desktop.audio.nothingPlaying')
        : t('desktop.audio.sheet.hear', { count: fh.heard.length }),
    );
    const first = fh.keptOut[0];
    if (first !== undefined) {
      parts.push(
        fh.keptOut.length === 1
          ? t('desktop.audio.sheet.keptOut', { app: first.name })
          : t('desktop.audio.sheet.keptOutMore', { app: first.name, count: fh.keptOut.length - 1 }),
      );
    }
  }
  return (
    <div className={styles.summary}>
      <p className={styles.summaryLine}>
        <span>{parts.join(' · ')}</span>{' '}
        <button
          type="button"
          className={styles.linkButton}
          aria-expanded={open}
          aria-controls={detailsId}
          onClick={() => {
            setOpen(!open);
          }}
        >
          {t('desktop.audio.sheet.details')}
        </button>
      </p>
      {open && (
        <div id={detailsId}>
          <AppGroups fh={fh} bridge={bridge} />
        </div>
      )}
    </div>
  );
}

/** FriendsHear.banners: always shown, never dismissable; error-level ones are alerts. */
export function AudioBanners({ fh }: { fh: FriendsHear }) {
  const { t } = useTranslation();
  const shown = fh.banners.map((b) => ({ b, text: bannerText(b, t) })).filter((x) => x.text !== '');
  if (shown.length === 0) return null;
  return (
    <div className={styles.banners}>
      {shown.map(({ b, text }) => (
        <p
          key={b.code}
          className={cx(styles.banner, styles[b.level])}
          role={b.level === 'error' ? 'alert' : 'status'}
          data-banner={b.code}
        >
          {text}
        </p>
      ))}
    </div>
  );
}

/** The mic prompts as cards: until answered, the app is kept out (05's default). */
export function MicPromptCards({ fh, bridge }: { fh: FriendsHear; bridge: NativeBridge | null }) {
  const { t } = useTranslation();
  const fail = useFailure();
  const [settled, setSettled] = useState<ReadonlySet<string>>(() => new Set());
  if (bridge === null) return null;
  const prompts = fh.prompts.filter((p) => !settled.has(p.id));
  if (prompts.length === 0) return null;
  const settle = (id: string, call: Promise<void>): void => {
    setSettled((s) => new Set(s).add(id));
    call.catch((err: unknown) => {
      // not_found: answered or dismissed elsewhere (a toast); the next report drops it anyway.
      if (bridgeErrorCode(err) !== 'not_found') {
        fail(err);
        setSettled((s) => {
          const next = new Set(s);
          next.delete(id);
          return next;
        });
      }
    });
  };
  return (
    <div className={styles.prompts}>
      {prompts.map((p) => {
        const text = promptTexts(p, t);
        const dismiss = (): void => {
          settle(p.id, bridge.audio.dismissPrompt(p.id));
        };
        // Esc on any of the card's buttons dismisses it, as its ✕ does.
        const onKeyDown = (e: KeyboardEvent): void => {
          if (e.key !== 'Escape') return;
          e.stopPropagation();
          e.preventDefault();
          dismiss();
        };
        return (
          <section
            key={p.id}
            className={styles.prompt}
            role="group"
            aria-label={text.question}
            data-prompt-kind={p.kind}
          >
            <p className={styles.question}>{text.question}</p>
            <div className={styles.promptActions}>
              <Button
                size="sm"
                variant="primary"
                onKeyDown={onKeyDown}
                onClick={() => {
                  settle(p.id, bridge.audio.decide(p.id, 'keep-out'));
                }}
              >
                {text.keepOut}
              </Button>
              <Button
                size="sm"
                onKeyDown={onKeyDown}
                onClick={() => {
                  settle(p.id, bridge.audio.decide(p.id, 'include'));
                }}
              >
                {text.include}
              </Button>
            </div>
            <button
              type="button"
              className={styles.close}
              aria-label={t('desktop.audio.prompt.dismiss')}
              onKeyDown={onKeyDown}
              onClick={dismiss}
            >
              <X aria-hidden="true" />
            </button>
          </section>
        );
      })}
    </div>
  );
}

/** FriendsHear.tips: the ducking tip with Open sound settings and its dismiss. */
export function AudioTips({
  fh,
  bridge,
  openSoundSettings,
}: {
  fh: FriendsHear;
  bridge: NativeBridge | null;
  openSoundSettings: () => void;
}) {
  const { t } = useTranslation();
  const fail = useFailure();
  // The only tip is ducking (05 §14.7).
  if (bridge === null || fh.tips.length === 0) return null;
  return (
    <div className={styles.tip} data-tip="ducking">
      <p>{t('desktop.audio.tip.ducking')}</p>
      <div className={styles.promptActions}>
        <Button size="sm" onClick={openSoundSettings}>
          {t('desktop.audio.openSoundSettings')}
        </Button>
        <Button
          size="sm"
          variant="ghost"
          onClick={() => {
            bridge.audio.dismissTip('ducking').catch(fail);
          }}
        >
          {t('desktop.audio.tip.dismiss')}
        </Button>
      </div>
    </div>
  );
}

/** "Friends won't hear: Discord, TeamSpeak, …. Apps that use your microphone are kept out too; you'll be asked." */
export function VoiceAppsLine({ fh }: { fh: FriendsHear }) {
  const { t } = useTranslation();
  if (fh.voiceApps.length === 0) return null;
  return (
    <p className={styles.note}>
      {t('desktop.audio.sheet.voiceApps', { apps: fh.voiceApps.map((a) => a.name).join(', ') })}
    </p>
  );
}

/** "Muting your PC doesn't mute your stream. Use Pause or turn Share sound off." */
export function VolumeNote() {
  const { t } = useTranslation();
  return <p className={styles.note}>{t('desktop.audio.sheet.volumeNote')}</p>;
}

/** The overall level meter of the mix (`mixPeakDb`). */
export function MixMeter({ levels }: { levels: Levels | null }) {
  const { t } = useTranslation();
  return (
    <meter
      className={styles.mix}
      min={METER_MIN_DB}
      max={METER_MAX_DB}
      value={meterValue(levels?.mixPeakDb)}
      aria-label={t('desktop.audio.mixLevel')}
    />
  );
}

/** "Nothing is playing right now", while sound is on and no heard row plays (docs/m2/03 §10.3). */
export function nothingPlaying(fh: FriendsHear, soundOn: boolean): boolean {
  return soundOn && fh.mode === 'exclude-apps' && fh.engine !== 'endpoint' && !fh.heard.some((r) => r.playing);
}
