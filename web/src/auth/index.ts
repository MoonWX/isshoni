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
//   PasswordField.tsx, AccountFields.tsx, AuthForm.tsx, Notice.tsx, useSubmit.ts, formErrors.ts
//                    the form pieces, shared with setup/SetupPage and the account pages
export { AboutPage } from './AboutPage';
export { InvitePage } from './InvitePage';
export { LoginPage } from './LoginPage';
export { PendingPage } from './PendingPage';
export { ResetPage } from './ResetPage';
export { SignupPage } from './SignupPage';
