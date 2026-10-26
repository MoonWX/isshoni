// The desktop app's Share button (docs/m2/03 §10.2), the host of the native share flow: the desktop ShareSheet, the
// Windows picker (Share.Pick), then the one-time sheets the pick names (PickedSource.confirm, in order: the
// first-share sheet, then the Do-not-disturb tip), then the share (the room session's startShare → Share.Start).
// The room page mounts it instead of M1's ShareButton when platform.sharing.mode is 'native', in the header and in
// the empty state, both on the page's shareStore, so one flow runs and one button shows its sheets (`hostId`).
//
// Failures (engineErrors.ts, §10.5): a pick Go no longer holds (pick_expired from Share.Start) opens the picker again
// without a word; a pick of an isshoni window (self_window) opens it again with "Pick something other than isshoni"
// above the sheet; every other failed pick is said in the sheet, with its action; a failed start is the share panel's
// to show (a toast when no panel is mounted). A pick is opened again by itself once per Share click; a second
// pick_expired in the same click is a failed start like the others ("That pick timed out…" with [Pick again]).
//
// As M1's button: it links the share state to the app's uiStore when it mounts (shareUi.ts), and gives the focus back
// when what its sheet started ended without a share.
import { MonitorUp } from 'lucide-react';
import { useEffect, useId, useRef, useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { useStore } from 'zustand';

import { useApp } from '../../app/context';
import type { Preset } from '../../protocol/types.gen';
import { Button, type ButtonStyleProps } from '../../ui/Button';
import type { ShareElsewhere } from '../ShareSheet';
import { shareStore, type ShareFlowOutcome, type ShareStore, type StartShare } from '../shareStore';
import { linkShareUi } from '../shareUi';
import { DndTip } from './DndTip';
import { isPickAgain, shareErrorText, shareErrorView, shareFailureText } from './engineErrors';
import { FirstShareSheet } from './FirstShareSheet';
import { NativeShareSheet } from './NativeShareSheet';

export interface NativeShareButtonProps extends ButtonStyleProps {
  /** Publishes the picked source: the room session's startShare (05 §11.1). */
  onStart: StartShare;
  /** Set while room.state has a share of this user from another tab or device (05 §13.1). */
  elsewhere?: ShareElsewhere | null;
  /** The button's text; default "Share". */
  label?: ReactNode;
  /** Default: the page's shareStore. */
  store?: ShareStore;
}

export function NativeShareButton({
  onStart,
  elsewhere = null,
  label,
  store = shareStore,
  variant = 'primary',
  size,
  block,
}: NativeShareButtonProps) {
  const { t } = useTranslation();
  const { platform, ui, prefs } = useApp();
  const sharing = platform.sharing;
  const phase = useStore(store, (s) => s.phase);
  const confirming = useStore(store, (s) => s.confirming);
  const picked = useStore(store, (s) => s.picked);
  const preset = useStore(store, (s) => s.preset);
  const hostId = useStore(store, (s) => s.hostId);
  const [sheetOpen, setSheetOpen] = useState(false);
  /** This button's own pick is open in the Windows picker. */
  const [picking, setPicking] = useState(false);
  /** A failed pick, said in the sheet. */
  const [pickError, setPickError] = useState<unknown>(null);
  const id = useId();
  const buttonRef = useRef<HTMLButtonElement>(null);
  const wantsFocusBack = useRef(false);
  /** The picker was opened again by itself in this Share click's flow (once at most). */
  const reopened = useRef(false);
  /** The last Share click's choices, for a pick opened again. */
  const lastChoice = useRef<{ preset: Preset; withAudio: boolean }>({ preset, withAudio: true });

  useEffect(() => (sharing ? store.getState().attach(id) : undefined), [store, sharing, id]);
  useEffect(() => {
    if (sharing) linkShareUi(store, ui);
  }, [store, sharing, ui]);

  // Focus back to the button when what its sheet started ended without a share (as M1's ShareButton, 05 §16.6).
  useEffect(() => {
    if (!wantsFocusBack.current || sheetOpen) return;
    if (phase === 'picking' || phase === 'confirming' || phase === 'starting') return;
    wantsFocusBack.current = false;
    if (phase !== 'idle' && phase !== 'failed') return;
    const active = document.activeElement;
    if (active === null || active === document.body) buttonRef.current?.focus();
  }, [phase, sheetOpen]);

  if (!sharing) return null;

  const openPicker = (choice: { preset: Preset; withAudio: boolean }): void => {
    lastChoice.current = choice;
    setPicking(true);
    setPickError(null);
    const pickingSource = sharing.pick({ preset: choice.preset, withAudio: choice.withAudio });
    void store.getState().pick(pickingSource, { preset: choice.preset, start: onStart }).then(settled);
  };

  /** Opens the picker again by itself (pick_expired, self_window): once per Share click. */
  const pickAgain = (err: unknown): boolean => {
    if (reopened.current) return false;
    reopened.current = true;
    if (store.getState().phase === 'failed') store.getState().dismiss();
    setSheetOpen(true);
    // self_window's line stays above the sheet while the picker is open again; pick_expired says nothing.
    openPicker(lastChoice.current);
    if (shareErrorText(err, t) !== '') setPickError(err);
    return true;
  };

  const settled = (outcome: ShareFlowOutcome): void => {
    setPicking(false);
    switch (outcome.step) {
      case 'cancelled':
        // Back to the sheet, which is still open (the picker closed without a choice).
        return;
      case 'confirming':
      case 'started':
        setSheetOpen(false);
        return;
      case 'failed': {
        const err = outcome.error;
        if (isPickAgain(err) && pickAgain(err)) return;
        const { panels, phase: now } = store.getState();
        if (now === 'failed') {
          // The start failed: the share panel shows it, or a toast without one.
          setSheetOpen(false);
          if (panels === 0) ui.getState().toast({ kind: 'error', message: shareFailureText(err, t) });
          return;
        }
        // The pick failed: said in the sheet, with what to do.
        setSheetOpen(true);
        setPickError(err);
        return;
      }
    }
  };

  const view = pickError !== null ? shareErrorView(pickError) : null;

  return (
    <>
      <Button
        ref={buttonRef}
        variant={variant}
        size={size}
        block={block}
        icon={<MonitorUp />}
        disabled={phase !== 'idle' && phase !== 'failed'}
        onClick={() => {
          wantsFocusBack.current = true;
          reopened.current = false;
          setPickError(null);
          setSheetOpen(true);
        }}
      >
        {label ?? t('share.button')}
      </Button>
      {sheetOpen && (
        <NativeShareSheet
          open
          picking={picking}
          error={pickError}
          elsewhere={elsewhere}
          onShare={(p, withAudio) => {
            reopened.current = false;
            openPicker({ preset: p, withAudio });
          }}
          onRetry={
            view?.action === 'capture-settings'
              ? () => {
                  openPicker(lastChoice.current);
                }
              : undefined
          }
          onClose={() => {
            setSheetOpen(false);
            setPickError(null);
          }}
        />
      )}
      {phase === 'confirming' && hostId === id && confirming === 'first-share' && (
        <FirstShareSheet
          open
          label={picked?.label ?? ''}
          preset={preset}
          onPreset={(p) => {
            store.getState().setPreset(p);
            // The sharer's last preset is the next share's default (05 §6.1).
            prefs.getState().setPreset(p);
          }}
          onGoLive={() => {
            void store.getState().confirm().then(settled);
          }}
          onCancel={() => {
            store.getState().cancel();
          }}
        />
      )}
      {phase === 'confirming' && hostId === id && confirming === 'dnd' && (
        <DndTip
          open
          onShareNow={() => {
            void store.getState().confirm().then(settled);
          }}
          onCancel={() => {
            store.getState().cancel();
          }}
        />
      )}
    </>
  );
}
