// The room page's `?focus=<shareId>` (05 §5, §12.2): read from the router's location, followed for up to 5 s
// (focusParam.ts), then dropped from the URL with a replace, so the Back button doesn't bring it back. ViewerLayout
// renders this inside a router; it shows nothing. A link that arrives while the page is open (a notification's
// click navigates the open tab) changes the location, and the new share is followed.
//
// The room page shows the layout while it is still joining, so this mounts before the room's shares are known:
// the 5 s start with the room's first snapshot, and the parameter stays in the URL until then.
import { useEffect, useEffectEvent } from 'react';
import { useSearchParams } from 'react-router';

import { FOCUS_PARAM, followFocusParam } from './focusParam';
import type { ViewerServices } from './services';

export interface FocusFromUrlProps {
  viewer: Pick<ViewerServices, 'store'>;
}

export function FocusFromUrl({ viewer }: FocusFromUrlProps) {
  const [params, setParams] = useSearchParams();
  const shareId = params.get(FOCUS_PARAM);

  const drop = useEffectEvent(() => {
    setParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        next.delete(FOCUS_PARAM);
        return next;
      },
      { replace: true },
    );
  });

  useEffect(() => {
    if (shareId === null) return undefined;
    if (shareId === '') {
      drop();
      return undefined;
    }
    return followFocusParam(viewer.store, shareId, drop);
  }, [viewer.store, shareId]);

  return null;
}
