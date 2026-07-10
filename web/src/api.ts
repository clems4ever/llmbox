// Typed client for the llmbox box-control API (/api/v1). Every mutating or
// listing call is a POST with a JSON body, authenticated by the browser's login
// cookie plus the session's CSRF token echoed in the X-CSRF-Token header — the
// same single API llmbox-mcp drives with a bearer key.

export interface Me {
  email: string;
  admin: boolean;
  csrf: string;
}

export interface SpokeStatus {
  name: string;
  connected: boolean;
  default?: boolean;
  enrolled_at?: string;
}

export interface JoinTokenInfo {
  id: string;
  name: string;
  // The box backend recorded when the token was created (docker/firecracker).
  backend: string;
  // The enrollment command with "<one-time-token>" in place of the secret —
  // the real token is shown only in the create response and never recoverable.
  command: string;
  expires_at: string;
}

export interface BoxView {
  instance_id: string;
  name: string;
  box_id?: string;
  description?: string;
  spoke?: string;
  image: string;
  // The backend state (running, exited, …), or the hub-derived "unreachable"
  // (spoke offline right now) / "terminated" (confirmed gone; tombstone).
  state: string;
  status: string;
  phase: string;
  created: number;
  // When the hub last observed the box on its spoke (Unix seconds, 0 = never).
  last_seen?: number;
  auth_url?: string;
  session_url?: string;
}

export interface ProxyInfo {
  box_id: string;
  port: number;
  url: string;
  slug: string;
  spoke?: string;
  description?: string;
}

export interface SpokeEnrollment {
  name: string;
  token: string;
  command: string;
}

/** ApiError carries the server's error message plus the HTTP status, so the app
 * can distinguish an expired session (401) from an ordinary failure. */
export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
  }
}

/** me fetches the current login session; it is the only cookie-only call (no
 * CSRF needed) and how the app bootstraps its API session. */
export async function me(): Promise<Me> {
  const resp = await fetch("/api/v1/me", { credentials: "same-origin" });
  if (!resp.ok) throw new ApiError(resp.status, await errorMessage(resp));
  return resp.json();
}

/** Api is the authenticated client: it stamps the session's CSRF token onto
 * every call. Construct it from the session returned by me(). */
export class Api {
  constructor(private csrf: string) {}

  private async call<T>(path: string, body: unknown): Promise<T> {
    const resp = await fetch(path, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "X-CSRF-Token": this.csrf,
      },
      credentials: "same-origin",
      body: JSON.stringify(body),
    });
    if (!resp.ok) throw new ApiError(resp.status, await errorMessage(resp));
    return resp.json();
  }

  async listBoxes(): Promise<BoxView[]> {
    const r = await this.call<{ boxes: BoxView[] | null }>("/api/v1/list-boxes", {});
    return r.boxes ?? [];
  }

  async spokeStatuses(): Promise<SpokeStatus[]> {
    const r = await this.call<{ spokes: SpokeStatus[] | null }>("/api/v1/spoke-statuses", {});
    return r.spokes ?? [];
  }

  async listJoinTokens(): Promise<JoinTokenInfo[]> {
    const r = await this.call<{ tokens: JoinTokenInfo[] | null }>("/api/v1/list-join-tokens", {});
    return r.tokens ?? [];
  }

  async proxyEnabled(): Promise<boolean> {
    const r = await this.call<{ enabled: boolean }>("/api/v1/proxy-enabled", {});
    return r.enabled;
  }

  async listProxies(): Promise<ProxyInfo[]> {
    const r = await this.call<{ proxies: ProxyInfo[] | null }>("/api/v1/list-proxies", {});
    return r.proxies ?? [];
  }

  async createSpoke(name: string, backend: string, ttl: string): Promise<SpokeEnrollment> {
    const r = await this.call<{ spoke: SpokeEnrollment }>("/api/v1/create-spoke", {
      name,
      backend,
      ttl,
    });
    return r.spoke;
  }

  dropSpoke(name: string): Promise<unknown> {
    return this.call("/api/v1/drop-spoke", { name });
  }

  setDefaultSpoke(name: string): Promise<unknown> {
    return this.call("/api/v1/set-default-spoke", { name });
  }

  revokeJoinToken(id: string): Promise<unknown> {
    return this.call("/api/v1/revoke-join-token", { id });
  }

  /** regenerateJoinToken swaps an outstanding join token for a freshly minted
   * one for the same spoke; the old token stops working and the new secret is
   * shown once, like a create. */
  async regenerateJoinToken(id: string): Promise<SpokeEnrollment> {
    const r = await this.call<{ spoke: SpokeEnrollment }>("/api/v1/regenerate-join-token", { id });
    return r.spoke;
  }

  async createBox(boxId: string, description: string, spoke: string): Promise<{ box_id: string; token: string }> {
    const r = await this.call<{ session: { BoxID: string; Token: string } }>("/api/v1/create-box", {
      opts: { BoxID: boxId, Description: description, SpokeName: spoke },
    });
    return { box_id: r.session.BoxID, token: r.session.Token };
  }

  async authPageURL(token: string): Promise<string> {
    const r = await this.call<{ url: string }>("/api/v1/auth-page-url", { token });
    return r.url;
  }

  destroyBox(boxId: string): Promise<unknown> {
    return this.call("/api/v1/destroy-box", { box_id: boxId });
  }

  async createProxy(boxId: string, port: number, description: string): Promise<ProxyInfo> {
    const r = await this.call<{ proxy: ProxyInfo }>("/api/v1/create-proxy", {
      box_id: boxId,
      port,
      description,
    });
    return r.proxy;
  }

  deleteProxy(boxId: string, port: number): Promise<unknown> {
    return this.call("/api/v1/delete-proxy", { box_id: boxId, port });
  }

  /** logout ends the browser's login session on the hub: the server deletes the
   * session and expires the login cookie, so the caller must then bounce to the
   * sign-in page. */
  logout(): Promise<unknown> {
    return this.call("/api/v1/logout", {});
  }
}

/** errorMessage extracts the {"error": ...} body of a failed API response,
 * falling back to the raw text or status line. */
async function errorMessage(resp: Response): Promise<string> {
  const text = await resp.text();
  try {
    const parsed = JSON.parse(text) as { error?: string };
    if (parsed.error) return parsed.error;
  } catch {
    // not JSON — fall through to the raw text
  }
  return text.trim() || `${resp.status} ${resp.statusText}`;
}
