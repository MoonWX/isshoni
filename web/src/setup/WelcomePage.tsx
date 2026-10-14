// /admin/welcome?step=2|3, the setup wizard's steps 2 and 3 (05 §14.1). Step 1, creating the admin, is SetupPage on
// /setup, which comes here when it is done; from then on the admin is signed in, and RequireAdmin guards the route.
// The step lives in the URL, so a reload never loses progress, and the page can be entered again later (the admin
// dashboard's setup checklist links here until setupWizardDone is set).
//
// 2. The connection test (conntest/'s ConnTestPanel, 05 §14.2), started at once. "Continue" is always there; it is
//    the main action after a green result, and secondary to the panel's "Test again" after anything else.
// 3. Invite friends. Entering the step creates an invite with the server's defaults (POST /api/v1/invites {}: 7 days,
//    10 uses unless the admin changed them) and shows its link once, in InviteLinkCard. "Done, go to the Lounge"
//    sets setupWizardDone (PATCH /api/v1/admin/settings, 03 §7.8) and opens the Lounge, the default room, also for
//    an admin who came back to the wizard after being in another room.
//
// The stepper shows all three steps, as on /setup.
import { useMutation, type QueryClient } from '@tanstack/react-query';
import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate, useSearchParams } from 'react-router';

import { useApp } from '../app/context';
import { CenteredPage } from '../app/layouts/CenteredPage';
import { focusPageHeading } from '../app/layouts/RootLayout';
import { Notice } from '../auth/Notice';
import { ConnTestPanel, runConnTest } from '../conntest';
import { errorMessage } from '../lib/errorText';
import type { CreateInviteRequest, CreateInviteResponse, Settings, SettingsResponse } from '../protocol/api.gen';
import { applyInvalidate } from '../protocol/invalidate';
import { queryKeys } from '../protocol/queryKeys';
import { api } from '../protocol/rest';
import { TopicAdminInvites, TopicAdminSettings, type Topic } from '../protocol/types.gen';
import { Button } from '../ui/Button';
import { Spinner } from '../ui/Spinner';
import { Stepper } from '../ui/Stepper';
import { InviteLinkCard } from './InviteLinkCard';
import styles from './WelcomePage.module.css';

/** The wizard's steps on this page; step 1 is SetupPage's. */
export type WelcomeStep = 2 | 3;

export const WELCOME_PATH = '/admin/welcome';

/** /admin/welcome?step=<n>: where the wizard shows a step. */
export function welcomePath(step: WelcomeStep): string {
  return `${WELCOME_PATH}?step=${String(step)}`;
}

/**
 * The step a `?step=` value asks for. Anything but 3 is step 2: the page without a step (the dashboard's checklist,
 * a hand-typed address) starts at the connection test, and step 1 is done once anyone can open this page.
 */
export function welcomeStep(raw: string | null): WelcomeStep {
  return raw === '3' ? 3 : 2;
}

/**
 * Where "Done" goes: the Lounge, as its label says (05 §14.1 "Done → Go to Lounge"). Not "/", whose root redirect
 * opens the last room the admin was in. The default room's ID is fixed and the room can't be deleted (03, owner
 * decision 2: `lounge`). A string, not roomPath of rooms/hooks, so the setup chunk doesn't pull in rooms code.
 */
export const AFTER_WIZARD_PATH = '/r/lounge';

/**
 * After a change went through: marks the REST data of the topic stale, exactly as the server's `invalidate` message
 * would (05 §6.2), and the audit log with it, where the change added a row.
 */
function refresh(queryClient: QueryClient, topic: Topic): void {
  void applyInvalidate(queryClient, { topics: [topic] });
  void queryClient.invalidateQueries({ queryKey: queryKeys.adminAudit });
}

export function WelcomePage() {
  const { t } = useTranslation();
  const [params] = useSearchParams();
  const step = welcomeStep(params.get('step'));

  // A new step is a new page with its own heading, under the same path: RootLayout moves focus only when the path
  // changes, so this does it for the step (05 §16.6). Not on the first render, and not twice under StrictMode.
  const shown = useRef(step);
  useEffect(() => {
    if (shown.current === step) return;
    shown.current = step;
    focusPageHeading();
  }, [step]);

  const stepper = (
    <Stepper
      label={t('setup.steps.label')}
      steps={[t('setup.steps.admin'), t('setup.steps.conntest'), t('setup.steps.invite')]}
      current={step - 1}
    />
  );
  return step === 2 ? <ConnTestStep stepper={stepper} /> : <InviteStep stepper={stepper} />;
}

interface StepProps {
  stepper: ReactNode;
}

/** Step 2: the connection test, run as soon as the step shows. */
function ConnTestStep({ stepper }: StepProps) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  /** The last finished run was green. False before the first result and while a run goes on. */
  const [green, setGreen] = useState(false);
  // The panel's own test, wrapped to see when a run starts: "Test again" after a green result makes the panel's
  // button the main one again, so "Continue" steps back until the new result is in.
  const runner = useCallback<typeof runConnTest>((platform, options) => {
    setGreen(false);
    return runConnTest(platform, options);
  }, []);

  return (
    <CenteredPage
      width="lg"
      title={t('setup.welcome.conntest.title')}
      lead={t('setup.welcome.conntest.lead')}
      banner={stepper}
    >
      <ConnTestPanel
        autoStart
        runner={runner}
        onResult={(_result, verdict) => {
          setGreen(verdict.status === 'green');
        }}
      />
      <div className={styles.actions}>
        <Button
          variant={green ? 'primary' : 'secondary'}
          onClick={() => {
            void navigate(welcomePath(3));
          }}
        >
          {t('setup.welcome.conntest.continue')}
        </Button>
      </div>
    </CenteredPage>
  );
}

/** Step 3: one invite with the server's defaults, its link shown once, and "Done". */
function InviteStep({ stepper }: StepProps) {
  const { t } = useTranslation();
  const { queryClient } = useApp();
  const navigate = useNavigate();
  /** The new invite and its link: in this page's state only (05 §20), gone when the admin leaves the step. */
  const [created, setCreated] = useState<CreateInviteResponse | null>(null);

  const create = useMutation({
    // The link is not kept after the step unmounts.
    gcTime: 0,
    // Sent at once, also when the browser says it is offline: the request then fails and the step says so.
    networkMode: 'always',
    mutationFn: () => {
      // Every field left out: the server's invite defaults (03 §12.4.5).
      const body: CreateInviteRequest = {};
      return api<CreateInviteResponse>('POST', '/api/v1/invites', body);
    },
    onSuccess: (res) => {
      setCreated(res);
      refresh(queryClient, TopicAdminInvites);
    },
  });
  const createInvite = create.mutate;
  // Entering the step creates the invite. On a timer, so React's StrictMode double mount in development creates
  // one invite, not two.
  useEffect(() => {
    const id = setTimeout(() => {
      createInvite();
    }, 0);
    return () => {
      clearTimeout(id);
    };
  }, [createInvite]);

  const finish = useMutation({
    gcTime: 0,
    networkMode: 'always',
    mutationFn: () => {
      const patch: Partial<Settings> = { setupWizardDone: true };
      return api<SettingsResponse>('PATCH', '/api/v1/admin/settings', patch);
    },
    onSuccess: async () => {
      refresh(queryClient, TopicAdminSettings);
      // Replaced: Back from the room doesn't come to a step whose link is gone.
      await navigate(AFTER_WIZARD_PATH, { replace: true });
    },
  });

  return (
    <CenteredPage
      width="lg"
      title={t('setup.welcome.invite.title')}
      lead={t('setup.welcome.invite.lead')}
      banner={stepper}
    >
      {created !== null ? (
        <InviteLinkCard created={created} />
      ) : create.isError ? (
        <Notice
          action={
            <Button
              size="sm"
              onClick={() => {
                createInvite();
              }}
            >
              {t('setup.welcome.invite.retry')}
            </Button>
          }
        >
          {t('setup.welcome.invite.failed', { message: errorMessage(create.error, t) })}
        </Notice>
      ) : (
        <p className={styles.creating}>
          <Spinner size="sm" label={null} />
          <span>{t('setup.welcome.invite.creating')}</span>
        </p>
      )}

      {finish.isError && (
        <Notice>{t('setup.welcome.invite.doneFailed', { message: errorMessage(finish.error, t) })}</Notice>
      )}
      <div className={styles.actions}>
        <Button
          variant="primary"
          loading={finish.isPending}
          onClick={() => {
            finish.mutate();
          }}
        >
          {t('setup.welcome.invite.done')}
        </Button>
      </div>
    </CenteredPage>
  );
}
