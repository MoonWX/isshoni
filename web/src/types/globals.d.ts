// Globals of the SPA's own (05 §4, §8, §17.1). This file is a script (no imports or exports), so its declarations
// are global.

/**
 * The build version: `ISSHONI_VERSION` at build time (06 passes the same string to the Go binary), default
 * `0.0.0-dev`. It is 01's `BUILD_VERSION`, which the SignalClient compares with `welcome.serverVersion`. Injected by
 * `define` in vite.config.ts, in builds, dev and Vitest alike.
 */
declare const __ISSHONI_VERSION__: string;

interface Window {
  /**
   * Set by `/boot-check.js` (a classic script that runs before the module entry) when this browser lacks what the
   * SPA needs; it has already written the "too old" message into `#root`, and main.tsx does nothing (05 §4).
   */
  __ISSHONI_UNSUPPORTED__?: boolean;
  /** Reserved name (05 §8): the desktop app's bridge, M2/M3. Nothing reads it in M1. */
  __ISSHONI_DESKTOP__?: unknown;
  /** Reserved name (05 §8): the mobile apps' runtime. Nothing reads it in M1. */
  Capacitor?: unknown;
}
