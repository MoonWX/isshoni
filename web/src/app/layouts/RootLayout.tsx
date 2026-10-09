// The root route's layout: renders the page and moves focus to the page's <h1> after each navigation (05 §16.6),
// so screen readers start at the new page. Pages own their structure (<main>, headers); the shell's toasts,
// announcer and update pill live outside the router (App.tsx).
//
// Above the page it shows what the router gives it as `above`: rooms/'s InRoomBar (05 §11.1), the room the tab is
// still in while the user is on another page. The bar renders nothing on a room's page and without a room. The
// router has it only once rooms/ is loaded (app/router.tsx): this file must not import rooms/ itself, or the room's
// code would be back in the entry chunk (05 §5).
import { useEffect, useRef, type ReactNode } from 'react';
import { Outlet, useLocation } from 'react-router';

import styles from './RootLayout.module.css';

/** Focuses the page heading: the first <h1> inside <main>, else the first <h1>. */
export function focusPageHeading(root: ParentNode = document): boolean {
  const h1 = root.querySelector<HTMLElement>('main h1') ?? root.querySelector<HTMLElement>('h1');
  if (!h1) return false;
  if (!h1.hasAttribute('tabindex')) h1.setAttribute('tabindex', '-1');
  h1.focus({ preventScroll: true });
  return true;
}

export interface RootLayoutProps {
  /** Shown above every page. */
  above?: ReactNode;
}

export function RootLayout({ above }: RootLayoutProps) {
  const { pathname } = useLocation();
  // Not on the first page load: focus starts at the top of the document, as usual. Comparing paths (not a
  // "first run" flag) also keeps StrictMode's second effect run from moving focus.
  const shown = useRef(pathname);
  useEffect(() => {
    if (shown.current === pathname) return;
    shown.current = pathname;
    focusPageHeading();
  }, [pathname]);
  return (
    <div className={styles.root}>
      {above}
      <Outlet />
    </div>
  );
}
