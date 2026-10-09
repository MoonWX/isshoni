// The download page folder's entry module (app/router.tsx's page-folder contract, 05 §5): the router loads this file
// as its own lazy chunk on the first visit to /download and picks the page by its export name. Only pages are
// exported here.
//
// The page's texts are part of the `account` namespace (account.download.*), which comes with the chunks that use it
// and not with the main bundle (05 §16.5): the import below adds it to the catalog before the router has the page.
import '../i18n/lazy/account';

export { DownloadPage } from './DownloadPage';
