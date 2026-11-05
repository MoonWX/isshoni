// The desktop app's parts of the app shell (docs/m2/03 §16.2, §16.3), loaded on demand by app/ in the desktop app
// only, so the browser never fetches them: the banner of a required update at the top of every page (App.tsx), the
// version screen's desktop variant (screens/VersionMismatch.tsx), and share/native's two parts that every page has
// (App.tsx): the close-to-tray notice at the top of the window (03 §11.6) and the mic prompts' corner card (03 §10.3). Their texts are the lazy `desktop` namespace,
// which the import below adds to the catalog before either renders (05 §16.5).
import '../i18n/lazy/desktop';

export { RequiredUpdateBanner } from './UpdateParts';
export { DesktopVersionMismatch } from './VersionScreen';
export { MicPromptCorner, WindowNotice } from '../share/native';
