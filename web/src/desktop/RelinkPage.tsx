// /desktop/relink (docs/m2/03 §8.3): the active server signed this PC out; Link again, Switch server, Remove
// server. RequireAuth sends the app here when the AuthState is unlinked with the notice of a refused refresh. A stub
// until WA71 (it renders M1's "Not in this build yet", so it adds no text of its own).
import { PageUnavailable } from '../app/NotFound';

export function RelinkPage() {
  return <PageUnavailable />;
}
