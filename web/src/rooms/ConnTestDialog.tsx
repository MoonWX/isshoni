// "Test my connection" in the room (05 §7.1, §9, §14.2): the connection test's panel in a dialog. The connection
// banner offers it after 30 s without the server, and the stage when the media port can't be reached. The panel is
// a chunk of its own, fetched when the dialog first opens: most visits never need it (05 §17.3).
//
// The chunk comes from the server, and this dialog is offered exactly when the server may be out of reach. So it is
// loaded here by hand instead of with React.lazy: a load that fails shows a message with a retry inside the dialog,
// where a failed lazy component would take the whole page, and the stream on it, to the error screen.
//
// The test runs when the user presses the panel's button, not when the dialog opens: it makes peer connections to
// the server, which the server limits per user (04 §7.7).
import { useEffect, useState, type ComponentType } from 'react';
import { useTranslation } from 'react-i18next';

import type { ConnTestPanelProps } from '../conntest/ConnTestPanel';
import { createLogger } from '../lib/log';
import { Button } from '../ui/Button';
import { Dialog } from '../ui/Dialog';
import { Spinner } from '../ui/Spinner';
import styles from './ConnTestDialog.module.css';

const log = createLogger('room');

type Panel = ComponentType<ConnTestPanelProps>;

type PanelState =
  { readonly status: 'loading' } | { readonly status: 'ready'; readonly Panel: Panel } | { readonly status: 'failed' };

function loadPanel(): Promise<Panel> {
  return import('../conntest/ConnTestPanel').then((m) => m.ConnTestPanel);
}

export interface ConnTestDialogProps {
  open: boolean;
  onClose: () => void;
  /** Loads the panel; default: the conntest chunk. Tests pass their own. */
  load?: () => Promise<Panel>;
}

export function ConnTestDialog({ open, onClose, load = loadPanel }: ConnTestDialogProps) {
  const { t } = useTranslation();
  const [panel, setPanel] = useState<PanelState>({ status: 'loading' });
  /** Grows with every "Try again". */
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    if (!open) return undefined;
    let current = true;
    load().then(
      (Panel) => {
        if (current) setPanel({ status: 'ready', Panel });
      },
      (err: unknown) => {
        log.warn('the connection test could not be loaded', { error: err });
        if (current) setPanel({ status: 'failed' });
      },
    );
    return () => {
      current = false;
    };
  }, [open, load, attempt]);

  return (
    <Dialog open={open} onClose={onClose} size="lg" title={t('room.connection.testConnection')}>
      {panel.status === 'ready' && <panel.Panel />}
      {panel.status === 'loading' && (
        <div className={styles.waiting}>
          <Spinner />
        </div>
      )}
      {panel.status === 'failed' && (
        <div className={styles.waiting}>
          <p role="alert">{t('room.connection.testUnavailable')}</p>
          <Button
            onClick={() => {
              setPanel({ status: 'loading' });
              setAttempt((n) => n + 1);
            }}
          >
            {t('room.connection.testRetry')}
          </Button>
        </div>
      )}
    </Dialog>
  );
}
