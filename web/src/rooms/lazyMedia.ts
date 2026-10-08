// The room page's components that live in the media chunk (rooms/media.ts), as React.lazy components: render them
// inside a <Suspense> whose fallback has their shape, so nothing moves when they arrive.
import { lazy } from 'react';

import { loadRoomMedia } from './loadMedia';

/** The stage and the tiles (RoomStage.tsx). */
export const RoomStage = lazy(() => loadRoomMedia().then((m) => ({ default: m.RoomStage })));

/** share/'s Share button, with the sheet and the warning it opens. */
export const ShareButton = lazy(() => loadRoomMedia().then((m) => ({ default: m.ShareButton })));
