// The `admin` namespace (05 §16.5): the admin pages' texts, in the chunk of the folder that imports this module
// (admin/index.ts) and added to the catalog when that chunk runs. See ../index.ts for the rules.
import { addMessages } from '../index';
import messages from './admin.en.json';

addMessages(messages);
