// Navigation side-effects isolated behind functions so the rest of the app never
// touches window.location directly — which lets specs spy on the redirect
// instead of triggering jsdom's unimplemented real navigation.

/** redirectToSignIn bounces the browser to the hub's server-rendered sign-in
 * page, asking it to return to /admin once the login cookie is re-established.
 * Called whenever the API answers 401 (no session, or one expired mid-action). */
export function redirectToSignIn(): void {
  window.location.href = "/signin?return=/admin";
}
