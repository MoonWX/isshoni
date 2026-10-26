// docs/m2/03 §10.5 and §22.3 "Engine errors": one case per row, by reason first and then by code.
import { describe, expect, it } from 'vitest';

import { i18n } from '../../i18n';
import { BridgeError } from '../../platform/desktop/errors';
import { ProtocolError } from '../../protocol/errors';
import { isPickAgain, shareErrorText, shareErrorView } from './engineErrors';

const t = i18n.t.bind(i18n);
const engine = (code: string, reason?: string) =>
  new BridgeError(`engine.${code}`, reason === undefined ? {} : { reason });

describe('shareErrorView: the reason rows win over their code', () => {
  it('pick_expired (Start’s source_gone, and the controller’s own code) reopens the picker without a word', () => {
    for (const err of [
      engine('source_gone', 'pick_expired'),
      new BridgeError('pick_expired', { reason: 'pick_expired' }),
    ]) {
      expect(shareErrorView(err)).toEqual({ key: null, action: 'pick-again' });
      expect(isPickAgain(err)).toBe(true);
      expect(shareErrorText(err, t)).toBe('');
    }
  });

  it('self_window reopens the picker with "Pick something other than isshoni"', () => {
    const err = engine('invalid', 'self_window');
    expect(shareErrorView(err)?.action).toBe('pick-again');
    expect(shareErrorText(err, t)).toBe('Pick something other than isshoni.');
  });

  it.each([
    ['driver_too_old', 'Update your graphics driver (NVIDIA 531.61 or newer).', 'desktop-gpu-driver'],
    ['no_media_foundation', 'Media Feature Pack', 'desktop-media-feature-pack'],
    ['blocked_by_policy', 'Smart App Control', 'desktop-smart-app-control'],
  ])('%s (as dependency) shows its text and its troubleshooting section', (reason, text, anchor) => {
    const err = engine('dependency', reason);
    expect(shareErrorView(err)).toMatchObject({ action: 'troubleshooting', anchor });
    expect(shareErrorText(err, t)).toContain(text);
  });

  it.each(['missing', 'version', 'abi'])('%s asks to reinstall, with the Download page', (reason) => {
    const err = engine('dependency', reason);
    expect(shareErrorView(err)?.action).toBe('download');
    expect(shareErrorText(err, t)).toContain('Reinstall isshoni');
  });

  it('device_removed: the driver restarted, and Share again starts a new pick', () => {
    const err = engine('encoder_lost', 'device_removed');
    expect(shareErrorView(err)?.action).toBe('share-again');
    expect(shareErrorText(err, t)).toBe('Your graphics driver restarted, so the share stopped.');
  });

  it('an unknown reason falls back to its code’s text', () => {
    expect(shareErrorText(engine('permission_denied', 'some_new_token'), t)).toBe(
      "Windows didn't allow isshoni to capture that.",
    );
  });
});

describe('shareErrorView: the code rows', () => {
  it('permission_denied: Windows settings and Try again', () => {
    expect(shareErrorView(engine('permission_denied'))?.action).toBe('capture-settings');
  });

  it('encoder_lost: the encoder stopped, Settings → Sharing', () => {
    expect(shareErrorView(engine('encoder_lost'))?.action).toBe('sharing-settings');
    expect(shareErrorText(engine('encoder_lost'), t)).toBe("Your PC's video encoder stopped working.");
  });

  it('source_gone, unsupported_os and no_encoder say what happened', () => {
    expect(shareErrorText(engine('source_gone'), t)).toBe('The window you shared was closed.');
    expect(shareErrorText(engine('unsupported_os'), t)).toBe('Sharing needs Windows 10 22H2 or later.');
    expect(shareErrorText(engine('no_encoder'), t)).toBe('No H.264 encoder works on this PC.');
    expect(shareErrorText(engine('dependency'), t)).toBe('No H.264 encoder works on this PC.');
  });

  it.each(['internal', 'invalid', 'busy', 'something_new'])('%s: something went wrong, with the logs', (code) => {
    expect(shareErrorView(engine(code))?.action).toBe('logs');
    expect(shareErrorText(engine(code), t)).toBe("Something went wrong in isshoni's video engine.");
  });

  it('cancelled is never said', () => {
    expect(shareErrorText(engine('cancelled'), t)).toBe('');
  });

  it('publisher_offline says why, with "See why"', () => {
    expect(shareErrorView(new BridgeError('publisher_offline'))).toMatchObject({ seeWhy: true });
  });

  it('the controller’s codes and the wire codes C2 passes on have their texts', () => {
    expect(shareErrorText(new BridgeError('already_sharing'), t)).toBe(
      "You're already sharing. Stop that share first.",
    );
    expect(shareErrorText(new BridgeError('room_full'), t)).toBe(t('errors.room_full'));
    expect(shareErrorText(new BridgeError('no_such_code'), t)).toBe(t('desktop.errors.internal'));
  });

  it('anything but a bridge error is M1’s', () => {
    const err = ProtocolError.local('connection_lost');
    expect(shareErrorView(err)).toBeNull();
    expect(shareErrorText(err, t)).toBe(t('errors.local.connection_lost'));
  });
});
