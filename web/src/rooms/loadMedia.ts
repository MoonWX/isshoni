// The one import() of the room's media chunk (rooms/media.ts). The module system keeps the loaded module, so every
// caller after the first gets it without a request: the room page's lazy components (lazyMedia.ts) and the
// session's sub PC (runtime.ts). No React here (05 §3).

/** Loads the room's media chunk, or returns it when it is loaded already. */
export function loadRoomMedia(): Promise<typeof import('./media')> {
  return import('./media');
}
