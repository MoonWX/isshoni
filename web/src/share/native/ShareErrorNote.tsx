// A native share's failure as the sheets and the panel say it (docs/m2/03 §10.5, engineErrors.ts): the text, with
// role="alert", and the one thing to do about it. Links go to the system browser (platform.openExternal): the project
// site's troubleshooting sections, and the server's Download page.
import type { ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router';

import { useApp } from '../../app/context';
import { DOCS_URL } from '../../conntest/links';
import { Button } from '../../ui/Button';
import { shareErrorText, shareErrorView } from './engineErrors';
import styles from './native.module.css';

export interface ShareErrorNoteProps {
  error: unknown;
  /** [Try again] (permission_denied): the picker opens again. Not offered without it. */
  onRetry?: (() => void) | undefined;
  /** [Share again] (device_removed): a new pick. Not offered without it. */
  onShareAgain?: (() => void) | undefined;
  /** Extra buttons after the error's own (the panel's Dismiss). */
  children?: ReactNode;
}

export function ShareErrorNote({ error, onRetry, onShareAgain, children }: ShareErrorNoteProps) {
  const { t } = useTranslation();
  const { platform } = useApp();
  const navigate = useNavigate();
  const view = shareErrorView(error);
  const text = shareErrorText(error, t);
  const bridge = platform.desktop?.bridge;
  const origin = platform.serverOrigin;

  const actions: ReactNode[] = [];
  switch (view?.action) {
    case 'capture-settings':
      if (bridge) {
        actions.push(
          <Button key="settings" size="sm" onClick={() => void bridge.app.openOSSettings('graphics-capture')}>
            {t('desktop.share.errors.captureSettings')}
          </Button>,
        );
      }
      if (onRetry) {
        actions.push(
          <Button key="retry" size="sm" onClick={onRetry}>
            {t('desktop.share.errors.tryAgain')}
          </Button>,
        );
      }
      break;
    case 'troubleshooting':
      actions.push(
        <Button
          key="help"
          size="sm"
          variant="ghost"
          onClick={() => {
            platform.openExternal(`${DOCS_URL}troubleshooting#${view.anchor ?? ''}`);
          }}
        >
          {t('desktop.share.errors.troubleshooting')}
        </Button>,
      );
      break;
    case 'download':
      if (origin !== '') {
        actions.push(
          <Button
            key="download"
            size="sm"
            onClick={() => {
              platform.openExternal(`${origin}/download`);
            }}
          >
            {t('desktop.share.errors.download')}
          </Button>,
        );
      }
      break;
    case 'share-again':
      if (onShareAgain) {
        actions.push(
          <Button key="again" size="sm" onClick={onShareAgain}>
            {t('desktop.share.errors.shareAgain')}
          </Button>,
        );
      }
      break;
    case 'sharing-settings':
      actions.push(
        <Button key="sharing" size="sm" onClick={() => void navigate('/desktop/settings#sharing')}>
          {t('desktop.share.errors.sharingSettings')}
        </Button>,
      );
      break;
    case 'logs':
      if (bridge) {
        actions.push(
          <Button key="logs" size="sm" onClick={() => void bridge.app.openLogsFolder().catch(() => undefined)}>
            {t('desktop.share.errors.logs')}
          </Button>,
        );
      }
      break;
    default:
      break;
  }
  if (view?.seeWhy === true) {
    actions.push(
      <Button
        key="why"
        size="sm"
        variant="ghost"
        onClick={() => {
          platform.openExternal(`${DOCS_URL}troubleshooting#desktop-linking`);
        }}
      >
        {t('desktop.share.errors.seeWhy')}
      </Button>,
    );
  }
  if (text === '' && actions.length === 0 && children === undefined) return null;
  return (
    <div className={styles.error}>
      {text !== '' && (
        <p className={styles.errorText} role="alert">
          {text}
        </p>
      )}
      {(actions.length > 0 || children !== undefined) && (
        <div className={styles.errorActions}>
          {actions}
          {children}
        </div>
      )}
    </div>
  );
}
