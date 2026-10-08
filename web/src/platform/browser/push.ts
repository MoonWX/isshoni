// STUB until S77 (05 W11 push) replaces this file.
//
// Contract for the replacement: BrowserPlatform imports createBrowserNotifications() from here and calls it once, so
// the export and its signature stay. S77 implements 05 §16.3: the support states (supported, needs-install on iOS
// in a Safari tab, denied, unsupported, and unsupported when /info has no push), enable (permission →
// pushManager.subscribe with info.push.vapidPublicKey → POST /api/v1/push/subscriptions), disable, test, and the
// re-post of the subscription on every app start once GET /api/v1/me has succeeded.
import { NotImplementedError } from '../../lib/errors';
import type { NotificationsProvider } from '../types';

/** The browser's NotificationsProvider. The stub reports no push support and rejects the actions. */
export function createBrowserNotifications(): NotificationsProvider {
  return {
    support: () => Promise.resolve('unsupported'),
    status: () => Promise.resolve('off'),
    enable: () => Promise.reject(new NotImplementedError('notifications.enable', 'S77')),
    disable: () => Promise.reject(new NotImplementedError('notifications.disable', 'S77')),
    test: () => Promise.reject(new NotImplementedError('notifications.test', 'S77')),
  };
}
