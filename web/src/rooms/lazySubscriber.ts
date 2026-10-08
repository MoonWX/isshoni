// The session's sub PC controller, loaded when it is first needed (05 §10.1: "The PC is created on the first
// pc.offer {pc: 'sub'}"). viewer/'s SubscriberPC lives in the room's media chunk (rooms/media.ts), not in the
// initial bundle; this stand-in is what RoomSession gets from SessionMedia.createSubscriber at once. It keeps the
// messages in the order they came until the real controller exists, and never makes one after close().
import type { PCICE, PCOffer } from '../protocol/types.gen';
import type { SubscriberLike } from './RoomSession';

/**
 * A SubscriberLike that makes the real one once its code is there.
 *
 * load() fetches the code and resolves with the function that makes the controller. It runs with the first message
 * (the server offers a sub PC only after the first subscription, so a session that never watches never loads it).
 * A load that fails rejects the messages that waited for it (the session logs that), and the next message loads
 * again: a chunk that didn't arrive once may arrive the next time.
 */
export function lazySubscriber(load: () => Promise<() => SubscriberLike>): SubscriberLike {
  let closed = false;
  let made: SubscriberLike | null = null;
  let ready: Promise<SubscriberLike | null> | null = null;

  const get = (): Promise<SubscriberLike | null> => {
    ready ??= load().then(
      (make) => {
        // close() came while the code was loading: the server's side is gone, so no PC is made at all.
        if (closed) return null;
        made = make();
        return made;
      },
      (err: unknown) => {
        ready = null;
        throw err;
      },
    );
    return ready;
  };

  // Every message waits on the same promise, and then() callbacks run in the order they were added: the offer
  // and the candidates that follow it reach the controller in the order they arrived.
  const pass = (handle: (sub: SubscriberLike) => Promise<void>): Promise<void> =>
    get().then((sub) => (sub === null || closed ? undefined : handle(sub)));

  return {
    handleOffer: (o: PCOffer) => pass((sub) => sub.handleOffer(o)),
    handleIce: (i: PCICE) => pass((sub) => sub.handleIce(i)),
    close() {
      closed = true;
      const sub = made;
      made = null;
      sub?.close();
    },
  };
}
