// The room page's components that live in the media chunk (rooms/media.ts), as React.lazy components: render them
// inside a <Suspense> whose fallback has their shape, so nothing moves when they arrive.
import { lazy } from 'react';

import { loadRoomMedia } from './loadMedia';

/** The stage and the tiles (RoomStage.tsx). */
export const RoomStage = lazy(() => loadRoomMedia().then((m) => ({ default: m.RoomStage })));

/** share/'s Share button, with the sheet and the warning it opens. */
export const ShareButton = lazy(() => loadRoomMedia().then((m) => ({ default: m.ShareButton })));

/** share/'s panel of this page's share (05 §13.7). It renders nothing while the page shares nothing. */
export const SharePanel = lazy(() => loadRoomMedia().then((m) => ({ default: m.SharePanel })));

/** viewer/'s "Tap to unmute" pill for the header (05 §10.3). It renders nothing while everything plays. */
export const TapToStartPill = lazy(() => loadRoomMedia().then((m) => ({ default: m.TapToStartPill })));
