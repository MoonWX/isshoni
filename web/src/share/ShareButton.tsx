// ShareButton (05 §11.2, §13.1, §13.8): the Share button with everything it opens: the ShareSheet, the browser's
// picker, and the ScreenAudioWarning when a whole screen came with system audio.
//
// The room page mounts it, in the header and in the empty state:
//   <ShareButton
//     onStart={(src, opts) => session.startShare(src, opts)}             // RoomSession, 05 §11.1
//     elsewhere={other && { onStop: () => signal.request('share.stop', { shareId: other.id }) }}
//   />                                                                   // other = findShareElsewhere(room.state…)
//
// It appears whenever the platform can share (platform.sharing, a capability probe: 05 §8), never by browser name.
// Phones and tablets have no getDisplayMedia; they get ShareUnavailableNote instead.
//
// Until the share panel exists (S46), this component also reports how the flow ended: a failed capture or a failed
// start as an error toast, and the "no sound is shared" notes of 05 §13.3 as an info toast.
import { MonitorUp } from 'lucide-react';
import { useEffect, useRef, useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { useStore } from 'zustand';

import { useApp } from '../app/context';
import { errorMessage } from '../lib/errorText';
import type { PickedSource } from '../platform/types';
import type { Preset } from '../protocol/types.gen';
import { Button, type ButtonStyleProps } from '../ui/Button';
import { noAudioNote } from './notes';
import { ScreenAudioWarning } from './ScreenAudioWarning';
import { ShareSheet, type ShareElsewhere } from './ShareSheet';
import { shareStore, type ShareFlowOutcome, type ShareStore, type StartShare } from './shareStore';

/** How long the "no sound is shared" note stays: longer than a plain toast, it tells the sharer what to change. */
export const NOTE_TOAST_MS = 10_000;

export interface ShareButtonProps extends ButtonStyleProps {
  /** Publishes the picked source: the room session's startShare (05 §11.1). */
  onStart: StartShare;
  /** Set while room.state has a share of this user from another tab or device (05 §13.1). */
  elsewhere?: ShareElsewhere | null;
  /** The button's text; default "Share". */
  label?: ReactNode;
  /** Default: the page's shareStore. */
  store?: ShareStore;
}

export function ShareButton({
  onStart,
  elsewhere = null,
  label,
  store = shareStore,
  variant = 'primary',
  size,
  block,
}: ShareButtonProps) {
  const { t } = useTranslation();
  const { platform, ui } = useApp();
  const sharing = platform.sharing;
  const phase = useStore(store, (s) => s.phase);
  const preset = useStore(store, (s) => s.preset);
  const pickId = useStore(store, (s) => s.pickId);
  const [sheetOpen, setSheetOpen] = useState(false);
  // The room page may show two Share buttons; only the one whose click started the current pick shows its warning.
  const [myPick, setMyPick] = useState<number | null>(null);
  const myPickRef = useRef<number | null>(null);

  // Leaving the page (the desktop-app link, a route change) while the picker or the warning of this button's pick
  // is open gives the pick up: the capture must not outlive the UI that explains it. cancel() leaves a share that
  // is already starting or live alone.
  useEffect(
    () => () => {
      if (store.getState().pickId === myPickRef.current) store.getState().cancel();
    },
    [store],
  );

  if (!sharing) return null;

  const follow = (step: Promise<ShareFlowOutcome>): void => {
    void step.then((outcome) => {
      if (outcome.step === 'failed') {
        ui.getState().toast({ kind: 'error', message: errorMessage(outcome.error, t) });
      } else if (outcome.step === 'started') {
        const note = noAudioNote(outcome.picked, t);
        if (note !== null) ui.getState().toast({ kind: 'info', message: note, durationMs: NOTE_TOAST_MS });
      }
    });
  };

  const pick = (picking: Promise<PickedSource | null>, withPreset: Preset): void => {
    setSheetOpen(false);
    const step = store.getState().pick(picking, { preset: withPreset, start: onStart });
    // pick() has counted this pick by now (it does so before its first await).
    myPickRef.current = store.getState().pickId;
    setMyPick(myPickRef.current);
    follow(step);
  };

  return (
    <>
      <Button
        variant={variant}
        size={size}
        block={block}
        icon={<MonitorUp />}
        // One share per page in M1: while one is being picked, started or live, there is nothing to start.
        disabled={phase !== 'idle' && phase !== 'failed'}
        onClick={() => {
          setSheetOpen(true);
        }}
      >
        {label ?? t('share.button')}
      </Button>
      {sheetOpen && (
        <ShareSheet
          open
          sharing={sharing}
          elsewhere={elsewhere}
          onPick={pick}
          onClose={() => {
            setSheetOpen(false);
          }}
        />
      )}
      {phase === 'confirming' && pickId === myPick && (
        <ScreenAudioWarning
          open
          sharing={sharing}
          preset={preset}
          onContinue={(withAudio) => {
            follow(store.getState().confirm(withAudio));
          }}
          onPickAgain={(picking) => {
            pick(picking, preset);
          }}
          onCancel={() => {
            store.getState().cancel();
          }}
        />
      )}
    </>
  );
}

/** What phones and tablets see where the Share button would be (05 §13.8). */
export function ShareUnavailableNote() {
  const { t } = useTranslation();
  return <p>{t('share.unavailable.phone')}</p>;
}
