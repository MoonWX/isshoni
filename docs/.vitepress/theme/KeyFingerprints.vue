<script setup lang="ts">
// The fingerprints of the release signing keys (06 §9.4, §10.3), from the latest release's facts. They must match
// the ones in the repository's README.md and the key that `ssh-keygen -Y verify` reports.
import { data as release } from '../release.data';

const repo = 'https://github.com/MoonWX/isshoni';
</script>

<template>
  <ul v-if="release">
    <li v-for="fp in release.keyFingerprints" :key="fp">
      <code>{{ fp }}</code>
    </li>
  </ul>
  <p v-else>
    None here yet: they appear with the first release. Until then the
    <a :href="`${repo}#release-signing-keys`">README.md</a> has them as soon as the keys exist.
  </p>
</template>
