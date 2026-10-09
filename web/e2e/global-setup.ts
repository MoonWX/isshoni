// Playwright's global setup (05 §19.3): it only finds the server binary. It starts no server: every e2e server
// comes from a fixture (fixtures.ts), so no spec depends on a server that another spec set up or restarted.
//
// The binary is ISSHONI_BIN (06 §14): `task e2e` and the CI e2e job set it to the absolute path of the built
// binary with the web app embedded. A relative value counts from the repository root, so
// `ISSHONI_BIN=bin/isshoni npx playwright test` works from web/ after a `task build`.
import { execFile } from 'node:child_process';
import path from 'node:path';
import { promisify } from 'node:util';

const run = promisify(execFile);

/** The repository root: this file is web/e2e/global-setup.ts. */
export const REPO_ROOT = path.resolve(import.meta.dirname, '..', '..');

/** The environment variable that names the server binary under test. */
export const BIN_ENV = 'ISSHONI_BIN';

/**
 * The absolute path of the binary that ISSHONI_BIN names; a relative value is resolved against the repository
 * root. Throws when the variable is unset or empty.
 */
export function resolveBinary(value: string | undefined = process.env[BIN_ENV]): string {
  if (value === undefined || value === '') {
    throw new Error(
      `${BIN_ENV} is not set. Run \`task e2e\` from the repository root (it builds bin/isshoni and sets ${BIN_ENV}), ` +
        `or set ${BIN_ENV} to a built binary; a relative path counts from the repository root.`,
    );
  }
  return path.resolve(REPO_ROOT, value);
}

/** What `isshoni version --json` prints (04 §15), as far as the harness reads it. */
export interface BuildInfo {
  readonly version: string;
  readonly commit: string;
}

/** Runs `isshoni version --json`. Throws with what to do when the binary is missing or does not run. */
export async function binaryVersion(bin: string): Promise<BuildInfo> {
  try {
    const { stdout } = await run(bin, ['version', '--json'], { timeout: 10_000 });
    const info = JSON.parse(stdout) as Partial<BuildInfo>;
    if (typeof info.version !== 'string' || typeof info.commit !== 'string') {
      throw new Error(`unexpected output: ${stdout.trim()}`);
    }
    return { version: info.version, commit: info.commit };
  } catch (err) {
    throw new Error(
      `${BIN_ENV}=${bin}: \`isshoni version --json\` did not run (${err instanceof Error ? err.message : String(err)}). ` +
        'Build the binary with `task build`, or run `task e2e`, which builds it first.',
      { cause: err },
    );
  }
}

/** A dev build is one whose SemVer prerelease starts with "dev" (06 §7.2): 0.0.0-dev+<commit>. */
function isDevBuild(version: string): boolean {
  return /^[^-+]*-dev(?:[.+-]|$)/.test(version);
}

export default async function globalSetup(): Promise<void> {
  const bin = resolveBinary();
  const info = await binaryVersion(bin);
  // The workers inherit the environment: the fixtures read the absolute path from here.
  process.env[BIN_ENV] = bin;
  console.log(`e2e: isshoni ${info.version} (${path.relative(REPO_ROOT, bin) || bin})`);
  if (isDevBuild(info.version)) {
    // 01's stale-build path only exists in a non-dev build (06 §8.2), and `task e2e` builds one.
    console.warn(`e2e: ${info.version} is a dev build: specs that need a release-like version fail (use \`task e2e\`)`);
  }
}
