// Compile-time checks of the seams between rooms/, viewer/ and share/ (npm run typecheck), like
// platform/types.test.ts. The three folders are built side by side against each other's contracts; the room runtime
// and the room page plug them together (runtime.ts, connectViewer.ts, RoomPage.tsx; 05 §11.2). Those would stop
// compiling too when a contract moves; these checks say which contract it was.
import { describe, expectTypeOf, it } from 'vitest';

import type { SignalClient } from '../protocol/signal-client';
import type { StartShare } from '../share/shareStore';
import type { attachViewer, syncRoom } from '../viewer/services';
import type { SubscriberDeps as SubscriberPCDeps, SubscriberPC } from '../viewer/SubscriberPC';
import type { createWatchToast } from '../viewer/watchToast';
import type { RoomEventTap, RoomSession, SessionMedia, SubscriberDeps } from './RoomSession';
import type { RoomStoreState } from './roomStore';

describe('seams of the room session', () => {
  it("viewer/'s SubscriberPC is the session's sub PC controller (SessionMedia.createSubscriber, 05 §10.1)", () => {
    type Factory = NonNullable<SessionMedia['createSubscriber']>;
    expectTypeOf<SubscriberPC>().toExtend<ReturnType<Factory>>();
    // `(deps) => new SubscriberPC({ ...deps, registry })`: the session's deps and the viewer's registry are all
    // that SubscriberPC needs; its store and ui are optional.
    expectTypeOf<SubscriberDeps & Pick<SubscriberPCDeps, 'registry'>>().toExtend<SubscriberPCDeps>();
  });

  it("the session's startShare is a ShareButton's onStart (05 §11.1, §13.1)", () => {
    expectTypeOf<RoomSession['startShare']>().toExtend<StartShare>();
  });

  it("viewer/'s watch toast is a room.event tap of the session (05 §12.2)", () => {
    expectTypeOf<ReturnType<typeof createWatchToast>>().toExtend<RoomEventTap>();
  });

  it("the room store's room.state is what the viewer syncs from, and the client is what it attaches to", () => {
    expectTypeOf<RoomStoreState['state']>().toExtend<Parameters<typeof syncRoom>[1]>();
    expectTypeOf<SignalClient>().toExtend<Parameters<typeof attachViewer>[0]>();
  });
});
