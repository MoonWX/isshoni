// The download page folder's entry module (app/router.tsx's page-folder contract, 05 §5): the router loads this file
// as its own lazy chunk on the first visit to /download and picks the page by its export name. Only pages are
// exported here.
export { DownloadPage } from './DownloadPage';
