// The default theme, plus the components that render the latest release's facts (release.data.ts) in the pages.
import DefaultTheme from 'vitepress/theme';
import type { Theme } from 'vitepress';
import KeyFingerprints from './KeyFingerprints.vue';
import ReleaseNotice from './ReleaseNotice.vue';

export default {
  extends: DefaultTheme,
  enhanceApp({ app }) {
    app.component('KeyFingerprints', KeyFingerprints);
    app.component('ReleaseNotice', ReleaseNotice);
  },
} satisfies Theme;
