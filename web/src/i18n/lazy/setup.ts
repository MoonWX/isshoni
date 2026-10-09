// The `setup` namespace (05 §16.5): the setup wizard's texts, in the chunk of the folder that imports this module
// (setup/index.ts) and added to the catalog when that chunk runs. See ../index.ts for the rules.
import { addMessages } from '../index';
import messages from './setup.en.json';

addMessages(messages);
