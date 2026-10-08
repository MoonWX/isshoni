// Stats (05 §10.7): the collector that samples the page's PCs, the summary of a getStats() report, and the debug
// handle the e2e tests read.
//
// Wiring, for whoever owns the room session (05 §11.1):
//   const stats = createStatsCollector({ sources: () => [...viewerStatsSources(viewer), ...pubSources], log });
//   stats.start() when the session joins its room, stats.stop() when it leaves;
//   installDebugHandle({ storage: platform.storage.session, stats: () => stats.snapshot(), state, dropSocket });
// The `stats` notification (01 §8.11) is toClientStats(stats.latest()) every STATS_REPORT_INTERVAL_MS; it and the
// debug overlay arrive with the hardening slice (05 W13).
export {
  createStatsCollector,
  STATS_FAST_INTERVAL_MS,
  STATS_HISTORY,
  STATS_INTERVAL_MS,
  type StatsCollector,
  type StatsCollectorDeps,
  type StatsSource,
} from './collector';
export {
  DEBUG_FLAG_KEY,
  debugEnabled,
  installDebugHandle,
  type DebugHandle,
  type DebugHandleDeps,
} from './debugHandle';
export {
  STATS_REPORT_INTERVAL_MS,
  summarize,
  toClientStats,
  type AudioInSample,
  type OutboundSample,
  type PCReport,
  type PCSample,
  type ShareSample,
  type StatsCounters,
  type StatsReportLike,
  type StatsSample,
  type StatsTrackRef,
  type Summary,
  type VideoInSample,
} from './summarize';
