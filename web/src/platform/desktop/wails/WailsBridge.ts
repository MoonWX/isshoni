// WailsBridge (docs/m2/03 §7.4): the NativeBridge over the generated Wails bindings (platform/desktop/bindings/) and
// @wailsio/runtime's Events.On, with rejections parsed into BridgeError (§6.4). It belongs to WA48, which declares the
// bound services it is generated from; until then this module is the loader's target and refuses to build a bridge,
// so every bridge call of a Wails build rejects with not_implemented.
//
// DesktopPlatform loads this module with a dynamic import() only when bridge.js says transport "wails"; it must stay
// out of the entry chunk (docs/m2/03 §7.2).
import type { NativeBridge } from '../bridge';
import { BridgeCodeNotImplemented, BridgeError } from '../errors';

export function createWailsBridge(): NativeBridge {
  throw new BridgeError(BridgeCodeNotImplemented, { transport: 'wails' });
}
