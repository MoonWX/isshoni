// The latest release, as .github/workflows/site.yml found it (docs/m1/06-deploy-and-ci.md §10.3, "06" below): its
// version, the SHA-256 of the install.sh this site serves, and the fingerprints of the release keys in
// deploy/keys/allowed_signers. site.yml writes release.json only when a release exists and its signature and
// checksums verified; without the file (every local build, and the site before the first release) `data` is null and
// the pages say "not released yet".
import { readFileSync } from 'node:fs';
import { defineLoader } from 'vitepress';

export interface Release {
  /** The release's version without the "v", e.g. 0.1.0. */
  version: string;
  /** SHA-256 (hex) of the release's install.sh, which the site serves as /install.sh. */
  installShSha256: string;
  /** `ssh-keygen -l` fingerprints of the keys in deploy/keys/allowed_signers, e.g. SHA256:…. */
  keyFingerprints: string[];
}

declare const data: Release | null;
export { data };

const versionRE = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(-rc\.[1-9]\d*)?$/;
const sha256RE = /^[0-9a-f]{64}$/;
const fingerprintRE = /^SHA256:[A-Za-z0-9+/]{43}$/;

export default defineLoader({
  watch: ['./release.json'],
  load(files: string[]): Release | null {
    if (files.length === 0) return null;
    const path = files[0];
    const raw: unknown = JSON.parse(readFileSync(path, 'utf8'));
    // A malformed file fails the build: the install and security pages would otherwise show wrong facts.
    const fail = (what: string): never => {
      throw new Error(`${path}: ${what} (written by .github/workflows/site.yml, docs/m1/06-deploy-and-ci.md §10.3)`);
    };
    if (typeof raw !== 'object' || raw === null) fail('not a JSON object');
    const r = raw as Record<string, unknown>;
    if (typeof r.version !== 'string' || !versionRE.test(r.version)) fail('version is not X.Y.Z or X.Y.Z-rc.N');
    if (typeof r.installShSha256 !== 'string' || !sha256RE.test(r.installShSha256)) {
      fail('installShSha256 is not a SHA-256 in hex');
    }
    const fps = r.keyFingerprints;
    if (!Array.isArray(fps) || fps.length === 0 || !fps.every((f) => typeof f === 'string' && fingerprintRE.test(f))) {
      fail('keyFingerprints is not a list of SHA256:… fingerprints');
    }
    return {
      version: r.version as string,
      installShSha256: r.installShSha256 as string,
      keyFingerprints: fps as string[],
    };
  },
});
