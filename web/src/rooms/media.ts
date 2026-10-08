// The room's media code, as one lazy chunk (05 §17.3: initial JS ≤ 200 KB): the stage with its tiles, the Share
// button with its sheet and warning, and the sub PC controller. The room page itself, with its header, is in the
// main chunk (05 §5); it asks for this chunk as soon as it renders, while the connection is still being made, so
// the stage is there before the first room.state. Only loadMedia.ts imports this file, with import().
//
// What later slices add to the viewer and the sharer UI (TapToStart, the watchers popover, the share panel) is
// imported here or below RoomStage, and stays out of the initial bundle the same way.
export { ShareButton } from '../share/ShareButton';
export { SubscriberPC } from '../viewer/SubscriberPC';
export { RoomStage } from './RoomStage';
