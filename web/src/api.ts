// Typed client for the llmbox box-control API (/api/v1). Every mutating or
// listing call is a POST with a JSON body, authenticated by the browser's login
// cookie plus the session's CSRF token echoed in the X-CSRF-Token header — the
// same single API that headless callers drive with a bearer key.

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
  // The box's provisioning phase: "broken" when its init script failed. A
  // healthy box omits the field (the API drops the empty phase), so it may be
  // absent — the single non-empty value the UI surfaces.
  phase?: string;
  // The failure detail for a broken box (phase "broken"): the init script's
  // captured output. Empty otherwise.
  last_error?: string;
  created: number;
  // When the hub last observed the box on its spoke (Unix seconds, 0 = never).
  last_seen?: number;
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

  /** createBox creates a workspace on the chosen runner. diskBytes is the
   * requested writable-disk size in bytes; omit it (or pass 0) to use the
   * runner's configured default. It is honoured only by microVM (Firecracker)
   * runners and clamped server-side to the runner's configured maximum. */
  async createBox(
    boxId: string,
    description: string,
    spoke: string,
    diskBytes = 0,
  ): Promise<{ box_id: string }> {
    const opts: { BoxID: string; Description: string; SpokeName: string; DiskBytes?: number } = {
      BoxID: boxId,
      Description: description,
      SpokeName: spoke,
    };
    if (diskBytes > 0) opts.DiskBytes = diskBytes;
    const r = await this.call<{ session: { BoxID: string } }>("/api/v1/create-box", { opts });
    return { box_id: r.session.BoxID };
  }

  destroyBox(boxId: string): Promise<unknown> {
    return this.call("/api/v1/destroy-box", { box_id: boxId });
  }

  /** pauseBox stops a box's compute to save CPU/RAM while keeping its disk, so it
   * can be resumed later. */
  pauseBox(boxId: string): Promise<unknown> {
    return this.call("/api/v1/pause-box", { box_id: boxId });
  }

  /** resumeBox restarts a paused box's compute; it comes back running, visible on
   * the next list refresh. */
  resumeBox(boxId: string): Promise<unknown> {
    return this.call("/api/v1/resume-box", { box_id: boxId });
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
