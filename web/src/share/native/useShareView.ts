// React's side of a DesktopShare (platform/desktop/DesktopShare.ts): its latest ShareView, and the encoder notice of
// share.notice for 5 s (docs/m2/03 §10.3, §10.5).
import { useCallback, useEffect, useState, useSyncExternalStore } from 'react';

import type { ShareView } from '../../platform/desktop/bridge';
import { isDesktopShare } from '../../platform/desktop/DesktopShare';
import type { ActiveShare } from '../../platform/types';

/** How long "Switched to <encoder>" shows (docs/m2/03 §10.5). */
export const ENCODER_NOTICE_MS = 5_000;

const noSubscribe = (): (() => void) => () => undefined;

/** The native share's latest view; null for no share, or one that isn't native. */
export function useShareView(share: ActiveShare | null): ShareView | null {
  const native = isDesktopShare(share) ? share : null;
  const subscribe = useCallback(
    (onChange: () => void) => (native ? native.on('view', onChange) : noSubscribe()),
    [native],
  );
  return useSyncExternalStore(subscribe, () => native?.view ?? null);
}

/** The encoder the engine fell back to (share.notice encoder_fallback), for ENCODER_NOTICE_MS; null otherwise. */
export function useEncoderNotice(share: ActiveShare | null): string | null {
  const [encoder, setEncoder] = useState<string | null>(null);
  useEffect(() => {
    if (!isDesktopShare(share)) return undefined;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const off = share.on('notice', (n) => {
      if (n.code !== 'encoder_fallback') return;
      const value = n.params?.['encoder'];
      clearTimeout(timer);
      setEncoder(typeof value === 'string' && value !== '' ? value : '?');
      timer = setTimeout(() => {
        setEncoder(null);
      }, ENCODER_NOTICE_MS);
    });
    return () => {
      off();
      clearTimeout(timer);
      setEncoder(null);
    };
  }, [share]);
  return encoder;
}
