// The derived local id vectors (docs/m2/03 §8.1: lowercase Crockford base32 of the first 5 bytes of
// SHA-256(origin), 8 characters). Go's servers.LocalID test (WA41) checks the same pairs; localId.node.test.ts derives
// them here, and DesktopPlatform.test.ts prefixes storage with them.
export const LocalIdVectors: readonly (readonly [origin: string, id: string])[] = [
  ['https://watch.example.com', 'j92qv4mz'],
  ['https://a.example.com', 'jfa4daer'],
  ['https://b.example.com', 'e9f3ch3x'],
  ['http://127.0.0.1:8080', 'tc55ev03'],
  ['https://xn--bcher-kva.example:8443', 'cj3v686e'],
];
