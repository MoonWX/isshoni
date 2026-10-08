// The "/" route (05 §5): → /r/<lastRoomId> if that room still exists, else /r/<defaultRoomId>, both from
// GET /api/v1/rooms. The last room is prefsStore.lastRoomId (the localStorage key isshoni.lastRoomId), which the
// session writes on every join. RequireAuth is around this route, so the user is signed in here.
import { Navigate } from 'react-router';

import { usePrefs } from '../app/context';
import { Fatal } from '../app/screens/Fatal';
import { Offline } from '../app/screens/Offline';
import { ApiError, isRetryableError } from '../protocol/rest';
import { PageSpinner } from '../ui/Spinner';
import { roomPath } from './hooks';
import { homeRoomId, useRooms } from './roomsQuery';

export function RootRedirect() {
  const rooms = useRooms();
  const lastRoomId = usePrefs((s) => s.lastRoomId);
  if (rooms.data !== undefined) return <Navigate to={roomPath(homeRoomId(rooms.data, lastRoomId))} replace />;
  if (rooms.isError) {
    // After the query's own retries, like the guards do for ['me'] (05 §6.2): try again, or give up.
    if (isRetryableError(rooms.error)) {
      return <Offline onRetry={() => void rooms.refetch()} retrying={rooms.isFetching} />;
    }
    return <Fatal code={rooms.error instanceof ApiError ? rooms.error.code : undefined} />;
  }
  return <PageSpinner />;
}
