// The `link` namespace (docs/m2/02-device-auth.md §10): the approval page's texts and the Devices page's app rows
// (link.app.<clientKind>.<os>, link.devices.*), in the chunk of the folders that import this module (link/index.ts,
// account/index.ts) and added to the catalog when such a chunk runs. See ../index.ts for the rules.
import { addMessages } from '../index';
import messages from './link.en.json';

addMessages(messages);
