import { Construction, SearchX } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router';

import { buttonClass } from '../ui/Button';
import { ScreenFrame } from './screens/ScreenFrame';

/**
 * The `*` route (05 §5), and what RequireAdmin/RequireInviter show to users who may not see a page, so admin pages
 * aren't advertised. /link (the M2 device approval page) renders it in M1 too.
 */
export function NotFound() {
  const { t } = useTranslation();
  return (
    <ScreenFrame
      icon={SearchX}
      title={t('common.notFound.title')}
      actions={
        <Link to="/" className={buttonClass({ variant: 'primary' })}>
          {t('common.goHome')}
        </Link>
      }
    >
      <p>{t('common.notFound.body')}</p>
    </ScreenFrame>
  );
}

/**
 * A route whose page folder hasn't been built yet (router.tsx: the folder's index.ts, or the page's export in it, is
 * missing). Page slices fill their folder and the route starts working without a router change.
 */
export function PageUnavailable() {
  const { t } = useTranslation();
  return (
    <ScreenFrame
      icon={Construction}
      title={t('common.pageUnavailable.title')}
      actions={
        <Link to="/" className={buttonClass({ variant: 'secondary' })}>
          {t('common.goHome')}
        </Link>
      }
    >
      <p>{t('common.pageUnavailable.body')}</p>
    </ScreenFrame>
  );
}
