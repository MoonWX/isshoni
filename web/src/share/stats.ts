// The sharer's side of the stats (05 §10.7): the page's pub PC as a source of lib/stats' collector, next to the
// viewer's sub PC (viewer/stats.ts). No React here (05 §3), and nothing heavy: the room's chunk imports this module
// for the collector's sources, while PublisherPC, which adds itself here, comes with the publisher's own chunk.
//
//   const collector = createStatsCollector({ sources: () => [...viewerStatsSources(viewer), ...shareStatsSources()] });
//
// What the collector makes of it: per sent layer and track the size, fps, rate, qualityLimitationReason and the
// encoder (OutboundSample), the PC's state and selected pair, which the `stats` notification (01 §8.11) and the debug
// overlay show. The sharer's upload and CPU hints keep their own sampling (BrowserShare, 05 §13.7).
import type { StatsSource } from '../lib/stats/collector';

/** Something that has a pub PC now and then: PublisherPC. */
export interface ShareStatsPublisher {
  /** The PC as the collector reads it while there is one; null while there is none. */
  statsSource(): StatsSource | null;
}

const publishers = new Set<ShareStatsPublisher>();

/** The page's pub PCs that exist now, for the collector's `sources` (asked at every sample). */
export function shareStatsSources(): StatsSource[] {
  const sources: StatsSource[] = [];
  for (const publisher of publishers) {
    const source = publisher.statsSource();
    if (source !== null) sources.push(source);
  }
  return sources;
}

/**
 * Lists a publisher among the page's sources: PublisherPC does it when it makes a PC. Listing it again changes
 * nothing. Returns the function that takes it off the list, which the publisher calls when its PC goes.
 */
export function addShareStatsPublisher(publisher: ShareStatsPublisher): () => void {
  publishers.add(publisher);
  return () => {
    publishers.delete(publisher);
  };
}
