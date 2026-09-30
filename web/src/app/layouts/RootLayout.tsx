// The root route's layout: renders the page and moves focus to the page's <h1> after each navigation (05 §16.6),
// so screen readers start at the new page. Pages own their structure (<main>, headers); the shell's toasts,
// announcer and update pill live outside the router (App.tsx).
import { useEffect, useRef } from 'react';
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

export function RootLayout() {
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
      <Outlet />
    </div>
  );
}
