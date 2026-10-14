// A click on a notification while the app is open (05 §16.2–16.3): the service worker focuses a window and posts
// {type: 'open', url} to it, and platform/browser/push.ts hands the URL (a path of this origin, checked) to the
// latest handler of onNotificationOpen(). This hook makes the router's navigate that handler while its page is
// mounted, so "Alex started sharing" opens the room with the share focused (`/r/lounge?focus=…`, which the viewer
// follows also on a page that is open already, 05 §12.2) without reloading the app. On a page that doesn't mount
// it, the provider navigates the whole page instead.
//
// The room page mounts it (through its lazy push prompts), and the notifications page, where the test
// notification's link points.
import { useEffect } from 'react';
import { useNavigate } from 'react-router';

import { onNotificationOpen } from '../platform/browser/push';

export function useNotificationOpen(): void {
  const navigate = useNavigate();
  useEffect(
    () =>
      onNotificationOpen((url) => {
        void navigate(url);
      }),
    [navigate],
  );
}
