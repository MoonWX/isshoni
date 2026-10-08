import { useTranslation } from 'react-i18next';
import { Link } from 'react-router';

import { CenteredPage } from '../app/layouts/CenteredPage';
import { buttonClass } from '../ui/Button';
import { Actions, Prose } from './AuthForm';

/**
 * /pending (05 §5): static. A sign-up in approval mode, or a login before an admin approved it, ends here. Pending
 * users get no session (03 §7.9), so the page can't tell when the approval happens: it says to try logging in later.
 */
export function PendingPage() {
  const { t } = useTranslation();
  return (
    <CenteredPage title={t('auth.pending.title')}>
      <Prose>
        <p>{t('auth.pending.body')}</p>
      </Prose>
      <Actions>
        <Link to="/login" className={buttonClass({ variant: 'secondary' })}>
          {t('auth.pending.back')}
        </Link>
      </Actions>
    </CenteredPage>
  );
}
