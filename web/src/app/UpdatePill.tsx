// STUB until S38 (05 W11 PWA shell) replaces this file.
//
// Contract for the replacement: the shell (App.tsx) mounts <UpdatePill /> once, outside the router, with no props.
// S38's version subscribes to platform.pwa.onUpdateReady (setting uiStore.updateReady) and, while an update waits
// and the user isn't sharing, offers "Update ready · Reload", which calls platform.pwa.applyUpdate() (05 §16.2).
//
// Until then no update can become ready (there is no service worker), so it renders nothing.

export function UpdatePill() {
  return null;
}
