// uiStore (05 §6.1): app-wide UI state that many parts write: toasts, the announcer's live regions, install
// availability, the update pill, and the app-level screen that replaces the router (a Fatal screen from a stopped
// connection, VersionMismatch). A vanilla Zustand store, so controllers that don't import React (RoomSession, the
// connection wiring) write it too; React reads it with useUi() (app/context.ts).
import { createStore, type StoreApi } from 'zustand/vanilla';

export type ToastKind = 'info' | 'success' | 'error';

export interface ToastAction {
  /** Already translated. */
  readonly label: string;
  readonly run: () => void;
}

export interface Toast {
  readonly id: number;
  readonly kind: ToastKind;
  /** Already translated. */
  readonly message: string;
  readonly action?: ToastAction;
  /** Auto-dismiss after this long; null keeps it until dismissed. Paused while hovered or focused. */
  readonly durationMs: number | null;
}

export type ToastInput = Omit<Toast, 'id' | 'durationMs'> & { durationMs?: number | null };

/** Why the Fatal screen shows (05 §4, §7.1): each has its own text; 'generic' is "Something went wrong". */
export type FatalReason = 'generic' | 'account_disabled' | 'too_many_connections' | 'media';

/**
 * A screen that replaces the whole app (05 §4). Boot shows offline, needsHttps, notSetUp and unsupported itself;
 * later parts set fatal and versionMismatch through showScreen().
 */
export type AppScreen =
  | { readonly kind: 'offline' }
  | { readonly kind: 'needsHttps' }
  | { readonly kind: 'notSetUp' }
  | { readonly kind: 'unsupported' }
  | { readonly kind: 'fatal'; readonly reason?: FatalReason; readonly code?: string }
  | {
      readonly kind: 'versionMismatch';
      /** The server's version (from welcome or the protocol_unsupported params). */
      readonly serverVersion?: string;
      /** The server is older than this page: the admin must update it (05 §16.4). */
      readonly serverOlder?: boolean;
      /** A reload already happened for this server version and didn't help. */
      readonly stillStale?: boolean;
    };

export type Politeness = 'polite' | 'assertive';

export interface Announcement {
  readonly id: number;
  readonly text: string;
}

export interface UiState {
  readonly toasts: readonly Toast[];
  /** The latest message of each live region (05 §16.6): polite for room events, assertive for errors. */
  readonly announcements: Readonly<Record<Politeness, Announcement | null>>;
  /** An install prompt is available (beforeinstallprompt kept, 05 §16.3). */
  readonly installAvailable: boolean;
  /** A new service worker waits: the UpdatePill offers "Update ready · Reload" (05 §16.2). */
  readonly updateReady: boolean;
  /** When set, this screen replaces the router. */
  readonly screen: AppScreen | null;

  /** Shows a toast; returns its id. Default duration: 5 s, errors 8 s. */
  toast(t: ToastInput): number;
  dismissToast(id: number): void;
  /** Sends text to a live region (screen readers only). */
  announce(text: string, politeness?: Politeness): void;
  setInstallAvailable(available: boolean): void;
  setUpdateReady(ready: boolean): void;
  showScreen(screen: AppScreen | null): void;
}

export type UiStore = StoreApi<UiState>;

/** At most this many toasts show at once; the oldest goes first. */
export const MAX_TOASTS = 4;

const DEFAULT_DURATION: Readonly<Record<ToastKind, number>> = { info: 5000, success: 5000, error: 8000 };

export function createUiStore(): UiStore {
  let nextId = 1;
  return createStore<UiState>()((set) => ({
    toasts: [],
    announcements: { polite: null, assertive: null },
    installAvailable: false,
    updateReady: false,
    screen: null,

    toast(t) {
      const id = nextId++;
      const toast: Toast = {
        ...t,
        id,
        durationMs: t.durationMs === undefined ? DEFAULT_DURATION[t.kind] : t.durationMs,
      };
      set((s) => ({ toasts: [...s.toasts, toast].slice(-MAX_TOASTS) }));
      return id;
    },
    dismissToast(id) {
      set((s) => ({ toasts: s.toasts.filter((t) => t.id !== id) }));
    },
    announce(text, politeness = 'polite') {
      const id = nextId++;
      set((s) => ({ announcements: { ...s.announcements, [politeness]: { id, text } } }));
    },
    setInstallAvailable(installAvailable) {
      set({ installAvailable });
    },
    setUpdateReady(updateReady) {
      set({ updateReady });
    },
    showScreen(screen) {
      set({ screen });
    },
  }));
}
