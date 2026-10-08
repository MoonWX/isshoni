import { Wrench } from 'lucide-react';
import { useTranslation } from 'react-i18next';

import { CommandBlock, ScreenFrame } from './ScreenFrame';

/** The commands that print the one-time setup link (04 §12.2); commands, not prose, so not translated. */
export const SETUP_URL_COMMAND = 'sudo isshoni setup-url';
export const SETUP_URL_DOCKER_COMMAND = 'docker compose exec isshoni isshoni setup-url';

/** /info says setupRequired and this isn't /setup (05 §4 step 4): the server's operator has to create the admin. */
export function NotSetUp() {
  const { t } = useTranslation();
  return (
    <ScreenFrame icon={Wrench} tone="warning" title={t('common.screens.notSetUp.title')}>
      <p>{t('common.screens.notSetUp.body')}</p>
      <CommandBlock>{SETUP_URL_COMMAND}</CommandBlock>
      <p>{t('common.screens.notSetUp.docker')}</p>
      <CommandBlock>{SETUP_URL_DOCKER_COMMAND}</CommandBlock>
      <p>{t('common.screens.notSetUp.after')}</p>
    </ScreenFrame>
  );
}
