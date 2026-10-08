// The room's media code, as one lazy chunk (05 §17.3: initial JS ≤ 200 KB): the stage with its tiles, the Share
// button with its sheet and warning, the share panel, the header's "Tap to unmute" pill, the sub PC controller and
// the stats collector. The room page itself, with its header, is in the main chunk (05 §5); it asks for this chunk
// as soon as it renders, while the connection is still being made, so the stage is there before the first
// room.state. Only loadMedia.ts imports this file, with import().
//
// What later slices add to the viewer and the sharer UI is imported here or below RoomStage, and stays out of the
// initial bundle the same way. A module of viewer/, share/ or lib/stats that the main chunk imports by itself
// (viewer/services.ts, the stores) must not import any of these.
export { createStatsCollector } from '../lib/stats/collector';
export { ShareButton } from '../share/ShareButton';
export { SharePanel } from '../share/SharePanel';
export { SubscriberPC } from '../viewer/SubscriberPC';
export { TapToStartPill } from '../viewer/TapToStart';
export { RoomStage } from './RoomStage';
