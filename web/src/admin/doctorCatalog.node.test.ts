// @vitest-environment node
// The doctor catalog (05 §15.3, §16.5; 04 §13.2): en.json has a text under doctor.<code> for every message code of
// doctor, and under doctor.fix.<fixCode> for every fix code, the ten certificate codes of 04 §8.7 among them. The
// codes come from the CLI's own templates, internal/server/ops/doctor/messages_en.go: the keys of its maps
// messagesEN and fixesEN (doctor.Codes() and doctor.FixCodes()), with the constants they name resolved in that file
// and in checks_net.go. A code that doctor gains without a text here fails this test; so does a text under doctor.
// that no code of doctor has (a typo, or a code that was renamed, which 04 forbids).
//
// The Doctor page renders those keys from the codes at run time (doctorText.ts), so check:i18n, which follows t('…')
// literals, can't see them; this is their check.
import { readFile } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';

import { beforeAll, describe, expect, it } from 'vitest';

import en from '../i18n/en.json';
import { CHECK_IDS } from './doctorChecks';

const DOCTOR_DIR = new URL('../../../internal/server/ops/doctor/', import.meta.url);

/** The string constants of Go source: `name = "value"`, in const blocks and alone. */
function constants(source: string): Map<string, string> {
  const out = new Map<string, string>();
  for (const m of source.matchAll(/^\s*(\w+)\s*=\s*"([^"\\]*)"/gm)) {
    const [, name, value] = m;
    if (name !== undefined && value !== undefined) out.set(name, value);
  }
  return out;
}

/** The keys of the Go map literal `var <name> = map[string]text{ … }`: its lines `\t<key>: …`, resolved. */
function mapKeys(source: string, name: string, consts: Map<string, string>): string[] {
  const start = source.indexOf(`var ${name} = map[string]text{`);
  if (start < 0) throw new Error(`messages_en.go has no map ${name}`);
  const end = source.indexOf('\n}\n', start);
  const body = source.slice(start, end);
  const keys: string[] = [];
  for (const m of body.matchAll(/^\t(\w+|"[^"]+"):\s/gm)) {
    const key = m[1] ?? '';
    const value = key.startsWith('"') ? key.slice(1, -1) : consts.get(key);
    if (value === undefined) throw new Error(`${name}: the key ${key} is not a string constant of the doctor package`);
    keys.push(value);
  }
  return keys;
}

type Tree = { readonly [key: string]: string | Tree };

/** The catalog's text at a dotted path; undefined when the path is no string. */
function textAt(tree: Tree, path: string): string | undefined {
  let node: string | Tree | undefined = tree;
  for (const part of path.split('.')) {
    if (node === undefined || typeof node === 'string') return undefined;
    node = node[part];
  }
  return typeof node === 'string' ? node : undefined;
}

/** Every string's dotted path under a tree. */
function leaves(tree: Tree, prefix = ''): string[] {
  return Object.entries(tree).flatMap(([key, value]) =>
    typeof value === 'string' ? [`${prefix}${key}`] : leaves(value, `${prefix}${key}.`),
  );
}

const catalog = en as unknown as Tree;
const doctor = catalog['doctor'] as Tree;

let codes: string[] = [];
let fixCodes: string[] = [];

beforeAll(async () => {
  const [messages, net] = await Promise.all([
    readFile(fileURLToPath(new URL('messages_en.go', DOCTOR_DIR)), 'utf8'),
    readFile(fileURLToPath(new URL('checks_net.go', DOCTOR_DIR)), 'utf8'),
  ]);
  const consts = new Map([...constants(net), ...constants(messages)]);
  codes = mapKeys(messages, 'messagesEN', consts);
  fixCodes = mapKeys(messages, 'fixesEN', consts);
});

describe('the doctor catalog', () => {
  it('reads the codes of the CLI', () => {
    // The two that README F04 and S60 made skips of a check's own, and a few of every kind.
    expect(codes).toEqual(
      expect.arrayContaining([
        'public_ip.loopback',
        'udp_buffers.unknown',
        'skip.needs_server',
        'config.ok',
        'bandwidth.near_nic',
      ]),
    );
    expect(fixCodes).toEqual(
      expect.arrayContaining(['config.fix_check', 'tls.acme_unreachable', 'tls.cert_expired', 'bandwidth.fix_link']),
    );
    expect(codes.length).toBeGreaterThan(90);
    expect(fixCodes.length).toBeGreaterThan(40);
  });

  it('has doctor.<code> for every message code', () => {
    const missing = codes.filter((code) => textAt(catalog, `doctor.${code}`) === undefined);
    expect(missing).toEqual([]);
  });

  it('has doctor.fix.<fixCode> for every fix code', () => {
    const missing = fixCodes.filter((code) => textAt(catalog, `doctor.fix.${code}`) === undefined);
    expect(missing).toEqual([]);
  });

  it('says why public_ip is skipped on a development server, without params (README F04)', () => {
    expect(textAt(catalog, 'doctor.public_ip.loopback')).toBe(
      'Not checked: network.include_loopback is on and network.stun_servers is empty ' +
        '(a development setup, which needs no public address).',
    );
    expect(textAt(catalog, 'doctor.public_ip.loopback')).not.toContain('{{');
    expect(textAt(catalog, 'doctor.udp_buffers.unknown')).toBe(
      "Not checked: net.core.rmem_max can't be read here, and isshoni is not running.",
    );
  });

  it('has no text that no code of doctor has', () => {
    // A variant is the code's key with a suffix: an i18next context (data_dir.ok_free) or plural (…_one).
    const belongs = (path: string, all: readonly string[]) =>
      all.some((code) => path === code || path.startsWith(`${code}_`));
    const { fix, part, ...messages } = doctor;
    const strays = [
      ...leaves(messages).filter((path) => !belongs(path, codes)),
      ...leaves(fix as Tree)
        .filter((path) => !belongs(path, fixCodes))
        .map((path) => `fix.${path}`),
    ];
    expect(strays).toEqual([]);
    expect(typeof part).toBe('object');
  });

  it('names every check of doctor on the page', async () => {
    const source = await readFile(fileURLToPath(new URL('doctor.go', DOCTOR_DIR)), 'utf8');
    const consts = constants(source);
    // checks() lists them in 04 §13.2's order: `{id: idConfig, run: checkConfig}`.
    const ids = [...source.matchAll(/\{id: (\w+),/g)].map((m) => consts.get(m[1] ?? '') ?? m[1]);
    expect(ids.length).toBeGreaterThan(10);
    expect([...CHECK_IDS]).toEqual(ids);
    const lazyAdmin = (await import('../i18n/lazy/admin.en.json')).default as unknown as Tree;
    const unnamed = ids.filter((id) => textAt(lazyAdmin, `admin.doctor.check.${id ?? ''}`) === undefined);
    expect(unnamed).toEqual([]);
  });
});
