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
// The buttons of a page share one flow (shareStore). The one in the empty state goes away as soon as a friend
// starts sharing, and the picker or the warning it opened must survive that: each mounted button attaches to the
// store as a host, the store names one of them to show the warning, and a pick is given up only when the last
// button unmounts.
//
// How the flow ended is told here as toasts: a failed capture always; a failed start and the "no sound is shared"
// notes of 05 §13.3 only on a page without a SharePanel, which shows both itself.
//
// Each button also links the share state to the app's uiStore when it mounts (shareUi.ts): a share can only start
// from a Share button, and what the rest of the app needs to know about it (the update pill must not offer a reload
// while sharing) has to go on when this page is left.
import { MonitorUp } from 'lucide-react';
import { useEffect, useId, useRef, useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { useStore } from 'zustand';

import { useApp } from '../app/context';
import { errorMessage } from '../lib/errorText';
import type { PickedSource } from '../platform/types';
import type { Preset } from '../protocol/types.gen';
import { Button, type ButtonStyleProps } from '../ui/Button';
import { primeLevelAudio } from './levelAudio';
import { noAudioNote } from './notes';
import { ScreenAudioWarning } from './ScreenAudioWarning';
import { ShareSheet, type ShareElsewhere } from './ShareSheet';
import { shareStore, type ShareFlowOutcome, type ShareStore, type StartShare } from './shareStore';
import { linkShareUi } from './shareUi';

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
  const hostId = useStore(store, (s) => s.hostId);
  const [sheetOpen, setSheetOpen] = useState(false);
  const id = useId();
  const buttonRef = useRef<HTMLButtonElement>(null);
  /** This button's sheet was opened, and what came of it hasn't ended yet: focus comes back here when it does. */
  const wantsFocusBack = useRef(false);

  // A host of the page's share flow while mounted. The store shows the warning on one host (hostId), whichever
  // button was clicked, and cancels a pick that hasn't started when the last host detaches: leaving the page (the
  // desktop-app link, a route change) while the picker or the warning is open gives the pick up, because the
  // capture must not outlive the UI that explains it.
  useEffect(() => (sharing ? store.getState().attach(id) : undefined), [store, sharing, id]);

  // uiStore.sharing follows the share state from here on, also after this button is gone (05 §16.2).
  useEffect(() => {
    if (sharing) linkShareUi(store, ui);
  }, [store, sharing, ui]);

  // Focus (05 §16.6). The sheet takes the focus, and it is unmounted while open: by its own Cancel, or by its Share
  // click, which also disables this button until the pick is settled. Either way the focus falls to <body>, and a
  // keyboard user would start again from the top of the page. So when what this button's sheet started is over
  // without a share (the sheet, the picker or the warning was cancelled, the capture or the start failed), the
  // button takes the focus back, unless it has gone somewhere else meanwhile. With two buttons on the page, that
  // is the one that was used, also when the other one showed the warning.
  useEffect(() => {
    if (!wantsFocusBack.current || sheetOpen) return;
    // Still under way: the picker or the warning is open, or the share is starting.
    if (phase === 'picking' || phase === 'confirming' || phase === 'starting') return;
    wantsFocusBack.current = false;
    // live and after: the share started. Where the focus goes when it ends is not this flow's business.
    if (phase !== 'idle' && phase !== 'failed') return;
    const active = document.activeElement;
    if (active === null || active === document.body) buttonRef.current?.focus();
  }, [phase, sheetOpen]);

  if (!sharing) return null;

  const follow = (step: Promise<ShareFlowOutcome>): void => {
    void step.then((outcome) => {
      // A mounted SharePanel shows a failed share (phase failed) and the note of a share without sound.
      const { panels, phase: now } = store.getState();
      if (outcome.step === 'failed') {
        if (panels > 0 && now === 'failed') return;
        ui.getState().toast({ kind: 'error', message: errorMessage(outcome.error, t) });
      } else if (outcome.step === 'started' && panels === 0) {
        const note = noAudioNote(outcome.picked, t);
        if (note !== null) ui.getState().toast({ kind: 'info', message: note, durationMs: NOTE_TOAST_MS });
      }
    });
  };

  const pick = (picking: Promise<PickedSource | null>, withPreset: Preset): void => {
    // Still inside the Share click (the sheet's, or the warning's "Pick something else"), after the picker was
    // opened: the level meter's AudioContext needs a user gesture to run (05 §13.7).
    primeLevelAudio();
    setSheetOpen(false);
    follow(store.getState().pick(picking, { preset: withPreset, start: onStart }));
  };

  return (
    <>
      <Button
        ref={buttonRef}
        variant={variant}
        size={size}
        block={block}
        icon={<MonitorUp />}
        // One share per page in M1: while one is being picked, started or live, there is nothing to start.
        disabled={phase !== 'idle' && phase !== 'failed'}
        onClick={() => {
          wantsFocusBack.current = true;
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
      {phase === 'confirming' && hostId === id && (
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
