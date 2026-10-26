// The desktop app's native share UI (share/native/, docs/m2/03 §10.2–§10.3) as React.lazy components, for the room
// page in the desktop (platform.sharing.mode 'native'). share/native/ is a chunk of its own: a browser never loads it.
// Render them inside a <Suspense> whose fallback has their shape.
import { lazy } from 'react';

const load = (): Promise<typeof import('../share/native')> => import('../share/native');

/** The desktop's Share button, with its ShareSheet, first-share sheet and Do-not-disturb tip. */
export const NativeShareButton = lazy(() => load().then((m) => ({ default: m.NativeShareButton })));

/** The desktop's share panel. It renders nothing while the app shares nothing. */
export const NativeSharePanel = lazy(() => load().then((m) => ({ default: m.NativeSharePanel })));

/** Where Share would be on a PC that can't share (Share.Capabilities' reason). */
export const NativeShareUnavailable = lazy(() => load().then((m) => ({ default: m.NativeShareUnavailable })));
