// The mic prompt card in the window's corner (docs/m2/03 §10.3): while the window is in front and no share panel or
// share sheet shows the mic prompts, 05's question ("KOOK is using your microphone. Keep it out of your stream?")
// would wait unseen (the Go side sends a toast only while the window isn't in front, 03 §15). This card shows it
// non-modally in a corner of every page of the desktop app, with the same buttons as the panel's card (Audio.Decide,
// Audio.DismissPrompt on ✕ and Esc).
//
// The panel and the sheets that render MicPromptCards count as hosts (useMicPromptHost); the corner shows only while
// none is mounted, so a prompt is never asked twice on screen.
//
// Mount it once for the whole desktop app (the desktop layout, inside the app's context), not per page.
import { useEffect, useSyncExternalStore } from 'react';
import { useTranslation } from 'react-i18next';

import styles from './audio.module.css';
import { MicPromptCards } from './FriendsHear';
import { useAudioBridge, useFriendsHear } from './useAudio';

let hosts = 0;
const listeners = new Set<() => void>();

function notify(): void {
  for (const l of listeners) l();
}

function subscribe(l: () => void): () => void {
  listeners.add(l);
  return () => {
    listeners.delete(l);
  };
}

function hosted(): boolean {
  return hosts > 0;
}

/** Marks a mounted component that shows the mic prompts itself while `active` (the panel, an open sheet). */
export function useMicPromptHost(active: boolean): void {
  useEffect(() => {
    if (!active) return undefined;
    hosts += 1;
    notify();
    return () => {
      hosts -= 1;
      notify();
    };
  }, [active]);
}

/** Whether a panel or an open sheet shows the mic prompts now. */
export function useMicPromptHosted(): boolean {
  return useSyncExternalStore(subscribe, hosted, hosted);
}

/** The mic prompts in the window's corner while nothing else shows them; nothing in the browser. */
export function MicPromptCorner() {
  const { t } = useTranslation();
  const bridge = useAudioBridge();
  const isHosted = useMicPromptHosted();
  const fh = useFriendsHear(bridge);
  if (bridge === null || fh === null || isHosted || fh.prompts.length === 0) return null;
  return (
    <div className={styles.corner} role="region" aria-label={t('desktop.audio.prompt.corner')}>
      <MicPromptCards fh={fh} bridge={bridge} />
    </div>
  );
}
