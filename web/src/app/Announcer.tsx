// The two live regions (05 §16.6): polite for room events ("alex started sharing", "Reconnected", "3 watching"),
// assertive for errors and "You're live". Anything writes through uiStore.announce(); the regions stay mounted for
// the page's life, so screen readers track them.
import { VisuallyHidden } from '../ui/VisuallyHidden';
import { useUi } from './context';

export function Announcer() {
  const polite = useUi((s) => s.announcements.polite);
  const assertive = useUi((s) => s.announcements.assertive);
  return (
    <>
      <VisuallyHidden aria-live="polite" aria-atomic="true" data-testid="announcer-polite">
        {/* A new element per announcement, so the same text twice is still read twice. */}
        {polite && <span key={polite.id}>{polite.text}</span>}
      </VisuallyHidden>
      <VisuallyHidden aria-live="assertive" aria-atomic="true" data-testid="announcer-assertive">
        {assertive && <span key={assertive.id}>{assertive.text}</span>}
      </VisuallyHidden>
    </>
  );
}
