// Typed client for the activation page's two endpoints: the JSON state the
// page renders from (GET /auth/{token}/state) and the code submission
// (POST /auth/{token}/code). Both are token-capability endpoints — no admin
// session is involved; when activation auth is enabled the hub gates the
// sensitive fields behind the login cookie instead.

export interface ProviderButton {
  label: string;
  login_path: string;
}

export interface AuthState {
  box_id?: string;
  spoke?: string;
  auth_enabled: boolean;
  logged_in: boolean;
  not_authorized?: boolean;
  email?: string;
  providers?: ProviderButton[];
  // Present only when the visitor may activate (auth disabled, or signed in
  // with the activate capability).
  status?: string;
  session_url?: string;
  error?: string;
  authorize_url?: string;
  csrf?: string;
}

/** tokenFromPath extracts the auth token from an activation page URL
 * (/auth/{token}).
 *
 * @arg pathname The page's location.pathname.
 * @return string The token segment, or "" when the path has none.
 */
export function tokenFromPath(pathname: string): string {
  const m = /^\/auth\/([^/]+)$/.exec(pathname);
  return m ? m[1] : "";
}

/** errorMessage extracts the hub's {"error": …} body, falling back to the
 * HTTP status text.
 *
 * @arg resp The failed response.
 * @return Promise<string> The message to show.
 */
async function errorMessage(resp: Response): Promise<string> {
  try {
    const body = (await resp.json()) as { error?: string };
    if (body.error) return body.error;
  } catch {
    // Not JSON; fall through to the status line.
  }
  return resp.statusText || `HTTP ${resp.status}`;
}

/** fetchAuthState loads the activation page's state for a token.
 *
 * @arg token The auth session token from the page URL.
 * @return Promise<AuthState> The gated state.
 * @throws Error with the hub's message (e.g. unknown or expired token).
 */
export async function fetchAuthState(token: string): Promise<AuthState> {
  const resp = await fetch(`/auth/${encodeURIComponent(token)}/state`, {
    credentials: "same-origin",
  });
  if (!resp.ok) throw new Error(await errorMessage(resp));
  return resp.json();
}

/** submitCode posts the pasted OAuth code (with the session CSRF token) and
 * returns the session's fresh state.
 *
 * @arg token The auth session token from the page URL.
 * @arg code The code the user pasted.
 * @arg csrf The CSRF token from the fetched state ("" when auth is disabled).
 * @return Promise<AuthState> The post-submit state.
 * @throws Error with the hub's message (401/403/… bodies).
 */
export async function submitCode(token: string, code: string, csrf: string): Promise<AuthState> {
  const resp = await fetch(`/auth/${encodeURIComponent(token)}/code`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    credentials: "same-origin",
    body: JSON.stringify({ code, csrf }),
  });
  if (!resp.ok) throw new Error(await errorMessage(resp));
  return resp.json();
}
