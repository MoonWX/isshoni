// @vitest-environment node
// The page's PowerShell command has the form of the script's own header (docs/m2/06-windows-release.md §7.1, §7.5):
// line 1 of deploy/install-desktop.ps1 shows the command slice R10 (WA49) chose, with "<your isshoni server>" for the
// origin, and the page must show the same form. A WF that adds the TLS 1.2 prefix changes both, or this fails.
import { readFile } from 'node:fs/promises';

import { describe, expect, it } from 'vitest';

import { ps1CommandFor, PS1_NEEDS_TLS12_PREFIX, PS1_TLS12_PREFIX } from './links';

const SCRIPT = new URL('../../../deploy/install-desktop.ps1', import.meta.url);

describe('the PowerShell command', () => {
  it("has the form of install-desktop.ps1's usage line", async () => {
    const [first = ''] = (await readFile(SCRIPT, 'utf8')).split('\n');
    const usage = ps1CommandFor('https://<your isshoni server>');
    expect(first).toBe(`# isshoni desktop installer for Windows. Usage: ${usage}`);
    expect(first.includes(PS1_TLS12_PREFIX.trim())).toBe(PS1_NEEDS_TLS12_PREFIX);
  });
});
