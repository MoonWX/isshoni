// WailsBridge (docs/m2/03 §7.4): the NativeBridge over the generated Wails bindings (platform/desktop/bindings/,
// written by `task gen:bindings` from internal/desktop/bridge and committed) and @wailsio/runtime's Events.On.
//
// - Every method calls the generated binding of the same Go method (group `auth` → service `Auth`, method
//   `accessToken` → `AccessToken`), so a renamed, removed or re-typed Go argument fails the typecheck here.
// - A failing Go method returns an error whose Error() is the bridge error JSON {"code", "params"}, and the pinned
//   Wails rejects the call with exactly that text as the message (W12); toBridgeError turns it into a BridgeError
//   (docs/m2/03 §6.4). A Wails TypeError or ReferenceError (a binding out of step with the Go side) becomes
//   `internal`.
// - The results are the Go services' JSON as the bindings' classes; their fields are those of bridge.ts's types,
//   which keep the string unions the Go side documents (the generated classes type them as plain strings).
// - The fire-and-forget methods (VoidMethods) return nothing, and their failures are logged, as HttpBridge does.
// - Events: Go emits "isshoni:<name>" (docs/m2/03 §6.3); `on(name, fn)` hands fn the event's data. share.stats gets
//   its receipt time `at` here.
//
// DesktopPlatform loads this module with a dynamic import() only when bridge.js says transport "wails"; it must stay
// out of the entry chunk with the Wails runtime and the bindings (docs/m2/03 §7.2).
import { Events } from '@wailsio/runtime';

import { createLogger } from '../../../lib/log';
import {
  App,
  Audio,
  Auth,
  Settings,
  Share,
  Updates,
  Window,
} from '../bindings/github.com/MoonWX/isshoni/internal/desktop/bridge';
import { BridgeEventPrefix, type BridgeEventName, type BridgeEvents, type NativeBridge } from '../bridge';
import { toBridgeError } from '../errors';

/** A bound call's result as the NativeBridge's type; a rejection becomes a BridgeError (docs/m2/03 §6.4). */
function call<T>(p: Promise<unknown>): Promise<T> {
  return p.then(
    (v) => v as T,
    (err: unknown) => {
      throw toBridgeError(err);
    },
  );
}

/** share.stats' sample gets its receipt time here (docs/m2/03 §6.3). */
function stamp<T>(v: T): T {
  return typeof v === 'object' && v !== null ? { ...v, at: performance.now() } : v;
}

export interface WailsBridgeOptions {
  /** @wailsio/runtime's Events.On; tests pass a fake. */
  on?: (name: string, fn: (ev: { data: unknown }) => void) => () => void;
}

export function createWailsBridge(opts: WailsBridgeOptions = {}): NativeBridge {
  const log = createLogger('desktop/wails-bridge');
  const listen = opts.on ?? ((name, fn) => Events.On(name, fn));

  /** A fire-and-forget call: its failure is logged, never thrown (VoidMethods). */
  const fire = (what: string, p: Promise<unknown>): void => {
    p.catch((err: unknown) => {
      log.warn('a bridge call failed', { call: what, code: toBridgeError(err).code });
    });
  };

  return {
    app: {
      info: () => call(App.Info()),
      openExternal: (url) => call(App.OpenExternal(url)),
      openOSSettings: (p) => call(App.OpenOSSettings(p)),
      quit: (stopShare) => call(App.Quit(stopShare)),
      log: (l, msg, attrs) => {
        fire('app.log', App.Log(l, msg, attrs ?? {}));
      },
      openLogsFolder: () => call(App.OpenLogsFolder()),
      openNotices: () => call(App.OpenNotices()),
      ownConnections: (ids) => {
        fire('app.ownConnections', App.OwnConnections(ids));
      },
      pendingLink: () => call(App.PendingLink()),
      linkHandled: () => call(App.LinkHandled()),
    },
    auth: {
      state: () => call(Auth.State()),
      servers: () => call(Auth.Servers()),
      probe: (address) => call(Auth.Probe(address)),
      addServer: (origin) => call(Auth.AddServer(origin)),
      switchServer: (origin) => call(Auth.SwitchServer(origin)),
      removeServer: (origin) => call(Auth.RemoveServer(origin)),
      link: () => call(Auth.Link()),
      cancelLink: () => call(Auth.CancelLink()),
      linkWithPassword: (username, password) => call(Auth.LinkWithPassword(username, password)),
      unlink: () => call(Auth.Unlink()),
      recheck: () => call(Auth.Recheck()),
      accessToken: (origin, force) => call(Auth.AccessToken(origin, force)),
      sessionEnded: (origin, code) => call(Auth.SessionEnded(origin, code)),
      helloRefused: (origin, e) => call(Auth.HelloRefused(origin, { code: e.code, params: e.params ?? {} })),
    },
    share: {
      capabilities: () => call(Share.Capabilities()),
      pick: () => call(Share.Pick()),
      release: (pickId) => {
        fire('share.release', Share.Release(pickId));
      },
      start: (r) => call(Share.Start(r)),
      current: () => call(Share.Current()),
      stop: () => call(Share.Stop()),
      stopOther: (roomId, shareId) => call(Share.StopOther(roomId, shareId)),
      setPaused: (p) => call(Share.SetPaused(p)),
      setPreset: (p) => call(Share.SetPreset(p)),
      setAudioEnabled: (on) => call(Share.SetAudioEnabled(on)),
      stats: () => call<BridgeEvents['share.stats'] | null>(Share.Stats()).then((v) => (v === null ? v : stamp(v))),
      watchServerStats: (on) => {
        fire('share.watchServerStats', Share.WatchServerStats(on));
      },
    },
    audio: {
      friendsHear: () => call(Audio.FriendsHear()),
      watch: (on) => {
        fire('audio.watch', Audio.Watch(on));
      },
      watchLevels: (on) => {
        fire('audio.watchLevels', Audio.WatchLevels(on));
      },
      setAppDecision: (appId, d) => call(Audio.SetAppDecision(appId, d)),
      setKeepOutMicUsers: (on) => call(Audio.SetKeepOutMicUsers(on)),
      decide: (promptId, d) => call(Audio.Decide(promptId, d)),
      dismissPrompt: (promptId) => call(Audio.DismissPrompt(promptId)),
      dismissTip: (code) => call(Audio.DismissTip(code)),
      appRules: () => call(Audio.AppRules()),
      soundCheck: () => call(Audio.SoundCheck()),
    },
    settings: {
      get: () => call(Settings.Get()),
      update: (p) => call(Settings.Update(p)),
      hotkeys: () => call(Settings.Hotkeys()),
      testHotkey: (combo) => call(Settings.TestHotkey(combo)),
    },
    window: { setFullscreen: (on) => call(Window.SetFullscreen(on)) },
    updates: {
      status: () => call(Updates.Status()),
      check: () => call(Updates.Check()),
      install: () => call(Updates.Install()),
    },
    on<E extends BridgeEventName>(e: E, fn: (p: BridgeEvents[E]) => void): () => void {
      return listen(BridgeEventPrefix + e, (ev) => {
        const data = (e === 'share.stats' ? stamp(ev.data) : ev.data) as BridgeEvents[E];
        try {
          fn(data);
        } catch (err) {
          log.error('a bridge event listener failed', { event: e, err });
        }
      });
    },
  };
}
