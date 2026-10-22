// The `desktop` namespace (docs/m2/03-desktop-app.md §18.1): the desktop app's own texts, in the chunk of the folders
// that import this module (desktop/index.ts, share/native/) and added to the catalog when such a chunk runs. Go reads
// the same file for its `desktop.native.*` keys (web.DesktopCatalog(), internal/desktop/i18n): the tray, the pill,
// toasts and native message boxes. See ../index.ts for the rules.
import { addMessages } from '../index';
import messages from './desktop.en.json';

addMessages(messages);
