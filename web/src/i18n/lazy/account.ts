// The `account` namespace (05 §16.5): the account pages' texts, with the download page's (account.download.*), in
// the chunk of the folders that import this module (account/index.ts, download/index.ts) and added to the catalog
// when such a chunk runs. See ../index.ts for the rules.
import { addMessages } from '../index';
import messages from './account.en.json';

addMessages(messages);
