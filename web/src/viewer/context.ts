// React's way to the viewer's objects: ViewerLayout provides them, and the components below it read the store with
// useViewer() and a share's received tracks with useShareMedia() (05 §6.1).
import { createContext, useCallback, useContext, useSyncExternalStore } from 'react';
import { useStore } from 'zustand';

import type { ShareMedia } from './mediaRegistry';
import type { ViewerServices } from './services';
import type { ViewerState } from './viewerStore';

export const ViewerContext = createContext<ViewerServices | null>(null);

export function useViewerServices(): ViewerServices {
  const viewer = useContext(ViewerContext);
  if (!viewer) throw new Error('useViewerServices: no ViewerContext (render inside <ViewerLayout>)');
  return viewer;
}

/** Reads viewerStore; re-renders when the selected value changes. */
export function useViewer<T>(selector: (s: ViewerState) => T): T {
  return useStore(useViewerServices().store, selector);
}

/** A share's received tracks from the media registry; re-renders when they change. */
export function useShareMedia(shareId: string): ShareMedia {
  const { registry } = useViewerServices();
  const subscribe = useCallback((onChange: () => void) => registry.subscribe(shareId, onChange), [registry, shareId]);
  return useSyncExternalStore(subscribe, () => registry.get(shareId));
}
