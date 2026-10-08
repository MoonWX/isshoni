// The auth page folder's entry module (app/router.tsx's page-folder contract): the router loads this file as one lazy
// chunk on the first visit to /login, /invite, /signup, /pending, /reset or /about, and picks each page by its
// export name. Only pages are exported here.
//
// Everything else in this folder is imported from its own file, never through this one, so that code in the main
// chunk doesn't pull the pages in with it:
//   useMe.ts         useMe()                     (the no-redirect read of GET /api/v1/me)
//   session.ts       startSession(), endSession()  (the cached identity after a sign-in and a logout)
//   logout.ts        logout(), addLogoutStep()   (no React: share/ and rooms/ register their steps)
//   useLogout.ts     useLogout()                 (menus, the account and devices pages)
//   loginNotice.ts   loginState()                (a navigation to /login that explains itself)
//   fragmentToken.ts useFragmentToken(), readFragmentToken()   (setup/SetupPage; the in-app browser banner's link)
//   InAppBrowserBanner.tsx  <InAppBrowserBanner />   (05 §16.3: the login and invite pages here, and the room page)
//   inAppBanner.ts   useInAppBanner()            (an in-app browser, and the banner not dismissed in this tab, whether
//                    or not the current page renders the banner: the Home Screen sheet and card wait for it. A page
//                    that hides its sheet or card on `showing` must render <InAppBrowserBanner /> itself, otherwise
//                    the user has nothing to dismiss)
//   PasswordField.tsx, AccountFields.tsx, AuthForm.tsx, Notice.tsx, useSubmit.ts, formErrors.ts
//                    the form pieces, shared with setup/SetupPage and the account pages
export { AboutPage } from './AboutPage';
export { InvitePage } from './InvitePage';
export { LoginPage } from './LoginPage';
export { PendingPage } from './PendingPage';
export { ResetPage } from './ResetPage';
export { SignupPage } from './SignupPage';
