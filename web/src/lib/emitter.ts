// A small typed event emitter for controllers that don't import React (05 §3 "Layering rule"): RoomSession,
// SubscriberPC, PublisherPC and ActiveShare report to the UI through Zustand stores and emitters like this one.

/** Event name → the listener's argument. */
export type EventMap = Record<string, unknown>;

export type Listener<T> = (value: T) => void;

export interface Emitter<E extends EventMap> {
  /** Adds a listener; the returned function removes it (calling it twice is harmless). */
  on<K extends keyof E>(event: K, fn: Listener<E[K]>): () => void;
  /** Adds a listener that runs at most once. */
  once<K extends keyof E>(event: K, fn: Listener<E[K]>): () => void;
  /**
   * Calls every listener of the event, in the order they were added, with the listeners present when emit started.
   * A listener that throws doesn't stop the others; its error is reported asynchronously (reportError), so it shows
   * up in the console and in tests without breaking the emitting controller.
   */
  emit<K extends keyof E>(event: K, value: E[K]): void;
  /** The number of listeners of one event, or of all events. */
  listenerCount(event?: keyof E): number;
  /** Removes every listener. */
  clear(): void;
}

export function createEmitter<E extends EventMap>(): Emitter<E> {
  const listeners = new Map<keyof E, Set<Listener<never>>>();

  const on = <K extends keyof E>(event: K, fn: Listener<E[K]>): (() => void) => {
    let set = listeners.get(event);
    if (!set) {
      set = new Set();
      listeners.set(event, set);
    }
    // A wrapper per registration, so the same function added twice is two listeners and each unsubscribe removes one.
    const entry: Listener<E[K]> = (v) => {
      fn(v);
    };
    set.add(entry);
    return () => {
      set.delete(entry);
    };
  };

  return {
    on,
    once(event, fn) {
      const off = on(event, (v) => {
        off();
        fn(v);
      });
      return off;
    },
    emit(event, value) {
      const set = listeners.get(event);
      if (!set || set.size === 0) return;
      for (const fn of [...set]) {
        try {
          (fn as Listener<typeof value>)(value);
        } catch (err) {
          rethrowLater(err);
        }
      }
    },
    listenerCount(event) {
      if (event !== undefined) return listeners.get(event)?.size ?? 0;
      let n = 0;
      for (const set of listeners.values()) n += set.size;
      return n;
    },
    clear() {
      listeners.clear();
    },
  };
}

function rethrowLater(err: unknown): void {
  if (typeof globalThis.reportError === 'function') {
    globalThis.reportError(err);
  } else {
    setTimeout(() => {
      throw err;
    });
  }
}
