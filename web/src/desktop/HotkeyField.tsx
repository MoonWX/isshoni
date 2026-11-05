// HotkeyField (docs/m2/03 §11.2, §14.4): one of the share's global hotkeys, Pause/Resume or Stop. It shows the saved
// combination and its status (Settings.Hotkeys(): registered while a share is live, idle otherwise, taken when
// another app holds it, invalid for a value this build can't read), and records a new one by key press: [Change],
// then the combination (hotkeys.ts's rules: two modifiers, or an F-key with one; never the Windows key; Escape
// cancels). Settings.TestHotkey checks the new one at once: Go registers and unregisters it, and answers taken, or
// typesChar when the combination types a character in the current keyboard layout, which the field warns about
// ("typing it during a share would trigger the hotkey") while saving it. A taken or invalid one isn't saved.
import { useEffect, useId, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';

import type { HotkeyStatus, NativeBridge, SettingsPatch } from '../platform/desktop/bridge';
import { Button } from '../ui/Button';
import { DEFAULT_HOTKEYS, readKey, type HotkeyId, type HotkeyProblem } from './hotkeys';
import { useBridgeFailure } from './SettingsParts';
import { isObject } from './settingsState';
import styles from './settings.module.css';

export interface HotkeyFieldProps {
  id: HotkeyId;
  bridge: NativeBridge;
  /** The saved combination (SettingsView.hotkeys[id]); null while unknown. */
  combo: string | null;
  /** The other hotkey's combination: the two must differ. */
  other: string | null;
  /** Its status from Settings.Hotkeys(), when known. */
  status: HotkeyStatus | null;
  /** Saves through Settings.Update. */
  save: (patch: SettingsPatch) => Promise<unknown>;
  /** Called after a save, so the statuses are read again. */
  onSaved: () => void;
}

/** What the field says under the combination. */
type Note =
  | { kind: 'problem'; problem: HotkeyProblem | 'same' | 'invalid' }
  | { kind: 'taken'; combo: string }
  | { kind: 'typesChar' };

export function HotkeyField({ id, bridge, combo, other, status, save, onSaved }: HotkeyFieldProps) {
  const { t } = useTranslation();
  const fail = useBridgeFailure();
  const [recording, setRecording] = useState(false);
  const [busy, setBusy] = useState(false);
  const [note, setNote] = useState<Note | null>(null);
  const recorder = useRef<HTMLInputElement>(null);
  const changeButton = useRef<HTMLButtonElement>(null);
  const noteId = useId();
  const refocus = useRef(false);

  useEffect(() => {
    if (recording) recorder.current?.focus();
    else if (refocus.current) {
      refocus.current = false;
      changeButton.current?.focus();
    }
  }, [recording]);

  const stop = (): void => {
    refocus.current = true;
    setRecording(false);
  };

  /** Tests a combination with Go, then saves it unless Go refuses it. */
  const apply = (next: string): void => {
    if (next === combo) {
      stop();
      return;
    }
    if (next === other) {
      setNote({ kind: 'problem', problem: 'same' });
      return;
    }
    setBusy(true);
    setNote(null);
    bridge.settings
      .testHotkey(next)
      .then(async (st) => {
        if (isObject(st) && st.state === 'taken') {
          setNote({ kind: 'taken', combo: next });
          return;
        }
        if (isObject(st) && st.state === 'invalid') {
          setNote({ kind: 'problem', problem: 'invalid' });
          return;
        }
        await save({ hotkeys: { [id]: next } });
        if (isObject(st) && st.typesChar === true) setNote({ kind: 'typesChar' });
        stop();
        onSaved();
      })
      .catch((err: unknown) => {
        fail(err);
      })
      .finally(() => {
        setBusy(false);
      });
  };

  const noteText = (n: Note): string => {
    switch (n.kind) {
      case 'taken':
        return t('desktop.settings.hotkeys.taken', { combo: n.combo });
      case 'typesChar':
        return t('desktop.settings.hotkeys.typesChar');
      case 'problem':
        return t(`desktop.settings.hotkeys.problem.${n.problem}`);
    }
  };

  const name = t(`desktop.settings.hotkeys.${id}`);
  const statusText = (() => {
    if (status === null) return null;
    switch (status.state) {
      case 'registered':
        return t('desktop.settings.hotkeys.status.registered');
      case 'idle':
        return t('desktop.settings.hotkeys.status.idle');
      case 'taken':
        return t('desktop.settings.hotkeys.taken', { combo: status.combo });
      case 'invalid':
        return t('desktop.settings.hotkeys.status.invalid');
    }
  })();

  return (
    <div className={styles.hotkey} data-hotkey={id}>
      <div className={styles.hotkeyHead}>
        <span>{name}</span>
        {recording ? (
          <input
            ref={recorder}
            className={styles.recorder}
            readOnly
            value=""
            placeholder={t('desktop.settings.hotkeys.press')}
            aria-label={t('desktop.settings.hotkeys.recordLabel', { name })}
            aria-describedby={note !== null ? noteId : undefined}
            aria-busy={busy || undefined}
            onBlur={() => {
              if (!busy) setRecording(false);
            }}
            onKeyDown={(ev) => {
              if (busy) {
                ev.preventDefault();
                return;
              }
              const r = readKey(ev);
              switch (r.kind) {
                case 'pass':
                  return;
                case 'pending':
                  ev.preventDefault();
                  return;
                case 'cancel':
                  ev.preventDefault();
                  setNote(null);
                  stop();
                  return;
                case 'problem':
                  ev.preventDefault();
                  setNote({ kind: 'problem', problem: r.problem });
                  return;
                case 'combo':
                  ev.preventDefault();
                  apply(r.combo);
                  return;
              }
            }}
          />
        ) : (
          <kbd className={styles.combo} data-testid={`hotkey-${id}`}>
            {combo ?? '…'}
          </kbd>
        )}
      </div>
      {statusText !== null && <p className={styles.muted}>{statusText}</p>}
      {note !== null && (
        <p id={noteId} role={note.kind === 'typesChar' ? 'status' : 'alert'} className={styles.warning}>
          {noteText(note)}
        </p>
      )}
      {!recording && (
        <div className={styles.actions}>
          <Button
            ref={changeButton}
            size="sm"
            disabled={combo === null}
            aria-label={t('desktop.settings.hotkeys.changeLabel', { name })}
            onClick={() => {
              setNote(null);
              setRecording(true);
            }}
          >
            {t('desktop.settings.hotkeys.change')}
          </Button>
          {combo !== null && combo !== DEFAULT_HOTKEYS[id] && (
            <Button
              size="sm"
              variant="ghost"
              disabled={busy}
              aria-label={t('desktop.settings.hotkeys.resetLabel', { name })}
              onClick={() => {
                apply(DEFAULT_HOTKEYS[id]);
              }}
            >
              {t('desktop.settings.hotkeys.reset')}
            </Button>
          )}
        </div>
      )}
    </div>
  );
}
