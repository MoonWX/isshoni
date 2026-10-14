<script setup lang="ts">
// Whether there is a release to install (06 §10.3): before the first one the site serves no install.sh, and the
// install pages say so; afterwards they name the version and the SHA-256 of the install.sh this site serves.
import { data as release } from '../release.data';

const repo = 'https://github.com/MoonWX/isshoni';
</script>

<template>
  <div v-if="release" class="tip custom-block">
    <p class="custom-block-title">Latest release: {{ release.version }}</p>
    <p>
      The install script this site serves is the one of
      <a :href="`${repo}/releases/tag/v${release.version}`">v{{ release.version }}</a>. Its SHA-256 is
      <code class="sha">{{ release.installShSha256 }}</code>
    </p>
  </div>
  <div v-else class="warning custom-block">
    <p class="custom-block-title">Not released yet</p>
    <p>
      isshoni has no release yet, so there is no install script to download: the commands on this page work once
      v0.1.0 is out. Until then isshoni runs from source, for development (<a :href="`${repo}/blob/main/CONTRIBUTING.md`"
        >CONTRIBUTING.md</a
      >).
    </p>
  </div>
</template>

<style scoped>
.sha {
  word-break: break-all;
}
</style>
