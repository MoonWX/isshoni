// The app's services, made once at boot and given to React through one context. Controllers that don't import React
// (RoomSession, the connection wiring) get the same objects passed in explicitly.
import type { QueryClient } from '@tanstack/react-query';
import { createContext, useContext } from 'react';
import { useStore } from 'zustand';

import type { Platform } from '../platform/types';
import { createPrefsStore, type PrefsState, type PrefsStore } from './prefs';
import { createUiStore, type UiState, type UiStore } from './uiStore';

export interface AppServices {
  readonly platform: Platform;
  readonly queryClient: QueryClient;
  readonly ui: UiStore;
  readonly prefs: PrefsStore;
}

export const AppContext = createContext<AppServices | null>(null);

/** Builds the stores for a platform (boot step 5; tests make their own). */
export function createAppServices(platform: Platform, queryClient: QueryClient): AppServices {
  return { platform, queryClient, ui: createUiStore(), prefs: createPrefsStore(platform.storage.local) };
}

export function useApp(): AppServices {
  const app = useContext(AppContext);
  if (!app) throw new Error('useApp: no AppContext (render inside <AppProviders>)');
  return app;
}

export function usePlatform(): Platform {
  return useApp().platform;
}

/** Reads uiStore; re-renders when the selected value changes. */
export function useUi<T>(selector: (s: UiState) => T): T {
  return useStore(useApp().ui, selector);
}

/** Reads prefsStore; re-renders when the selected value changes. */
export function usePrefs<T>(selector: (s: PrefsState) => T): T {
  return useStore(useApp().prefs, selector);
}
