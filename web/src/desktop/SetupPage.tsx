// /desktop/setup (docs/m2/03 §8.2): the first-run wizard with its server, add, confirm and link steps, then the
// sound and voice steps (DesktopFeatures.wizardSteps()). A stub until WA71: the route, its guard and its chunk are
// WA42's, the page is not (it renders M1's "Not in this build yet", so it adds no text of its own).
import { PageUnavailable } from '../app/NotFound';

export function SetupPage() {
  return <PageUnavailable />;
}
