// The connection test (05 §14.2): what other folders import. The wizard's step 2 (setup/), the doctor page (admin/)
// and "Test my connection" render ConnTestPanel; controllers that need the bare result call runConnTest.
// This folder is not a page folder (app/router.tsx loads none of it), so a lazy page that imports it takes it into
// its own chunk. Code that must not import React (controllers, 05 §3) imports ./runConnTest directly.
export { ConnTestPanel, type ConnTestPanelProps } from './ConnTestPanel';
export {
  fixText,
  knownNat,
  NAT_KINDS,
  natKey,
  providerKey,
  type FixLine,
  type FixLineId,
  type FixText,
  type FixValues,
} from './fixText';
export {
  CLOUD_PROVIDERS,
  CT_CODES,
  DOCS_URL,
  knownProvider,
  troubleshootingUrl,
  vpsGuideUrl,
  type CtCode,
} from './links';
export {
  runConnTest,
  type ConnTestResult,
  type ProbeTransport,
  type ProbeVerdict,
  type RunConnTestOptions,
} from './runConnTest';
export {
  rttLabelOf,
  verdictOf,
  type ConnTestStatus,
  type ConnTestVerdict,
  type RowState,
  type RttLabel,
  type TransportRow,
} from './verdict';
