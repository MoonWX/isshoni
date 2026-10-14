// Whether the page is visible now (the Page Visibility API): false in a background tab, a minimized window, or a
// phone's locked screen. The dashboard polls only while it is true (05 §15.3).
import { useSyncExternalStore } from 'react';

function subscribe(onChange: () => void): () => void {
  document.addEventListener('visibilitychange', onChange);
  return () => {
    document.removeEventListener('visibilitychange', onChange);
  };
}

function visible(): boolean {
  return document.visibilityState !== 'hidden';
}

export function usePageVisible(): boolean {
  return useSyncExternalStore(subscribe, visible, () => true);
}
