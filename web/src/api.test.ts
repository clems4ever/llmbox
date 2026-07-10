import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { Api, ApiError, me } from "./api";

/** jsonResponse fakes a fetch Response carrying a JSON body. */
function jsonResponse(body: unknown, init: { ok?: boolean; status?: number } = {}): Response {
  const text = JSON.stringify(body);
  return {
    ok: init.ok ?? true,
    status: init.status ?? 200,
    statusText: "OK",
    json: async () => JSON.parse(text),
    text: async () => text,
  } as Response;
}

/** textResponse fakes a non-JSON error Response. */
function textResponse(text: string, status: number): Response {
  return {
    ok: false,
    status,
    statusText: "Error",
    json: async () => JSON.parse(text),
    text: async () => text,
  } as Response;
}

const fetchMock = vi.fn();

beforeEach(() => {
  vi.stubGlobal("fetch", fetchMock);
  fetchMock.mockReset();
});
afterEach(() => vi.unstubAllGlobals());

describe("me", () => {
  it("returns the session on success", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ email: "a@b.c", admin: true, csrf: "x" }));
    const session = await me();
    expect(session.email).toBe("a@b.c");
    expect(fetchMock).toHaveBeenCalledWith("/api/v1/me", { credentials: "same-origin" });
  });

  it("throws ApiError with the status on failure", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ error: "no session" }, { ok: false, status: 401 }));
    await expect(me()).rejects.toMatchObject({ status: 401, message: "no session" });
    await expect(me()).rejects.toBeInstanceOf(ApiError);
  });
});

describe("Api.call", () => {
  it("posts JSON with the CSRF header and unwraps the result", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ boxes: [{ box_id: "b" }] }));
    const api = new Api("csrf-token");
    const boxes = await api.listBoxes();
    expect(boxes).toEqual([{ box_id: "b" }]);
    const [path, opts] = fetchMock.mock.calls[0];
    expect(path).toBe("/api/v1/list-boxes");
    expect(opts.method).toBe("POST");
    expect(opts.headers["X-CSRF-Token"]).toBe("csrf-token");
    expect(opts.headers["Content-Type"]).toBe("application/json");
  });

  it("defaults null list bodies to empty arrays", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ boxes: null }));
    expect(await new Api("t").listBoxes()).toEqual([]);
    fetchMock.mockResolvedValue(jsonResponse({ spokes: null }));
    expect(await new Api("t").spokeStatuses()).toEqual([]);
    fetchMock.mockResolvedValue(jsonResponse({ tokens: null }));
    expect(await new Api("t").listJoinTokens()).toEqual([]);
    fetchMock.mockResolvedValue(jsonResponse({ proxies: null }));
    expect(await new Api("t").listProxies()).toEqual([]);
  });

  it("posts logout to the session endpoint", async () => {
    fetchMock.mockResolvedValue(jsonResponse({}));
    await new Api("csrf-token").logout();
    const [path, opts] = fetchMock.mock.calls[0];
    expect(path).toBe("/api/v1/logout");
    expect(opts.method).toBe("POST");
    expect(opts.headers["X-CSRF-Token"]).toBe("csrf-token");
  });

  it("extracts the {error} body of a failed call", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ error: "boom" }, { ok: false, status: 500 }));
    await expect(new Api("t").destroyBox("x")).rejects.toMatchObject({ status: 500, message: "boom" });
  });

  it("falls back to raw text when the error body is not JSON", async () => {
    fetchMock.mockResolvedValue(textResponse("plain failure", 502));
    await expect(new Api("t").destroyBox("x")).rejects.toMatchObject({ message: "plain failure" });
  });

  it("falls back to the status line when the body is empty", async () => {
    fetchMock.mockResolvedValue({
      ok: false,
      status: 503,
      statusText: "Service Unavailable",
      text: async () => "",
    } as Response);
    await expect(new Api("t").destroyBox("x")).rejects.toMatchObject({ message: "503 Service Unavailable" });
  });
});

describe("Api mutations", () => {
  it("createSpoke returns the enrollment and sends the fields", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ spoke: { name: "e1", token: "tk", command: "cmd" } }));
    const out = await new Api("t").createSpoke("e1", "docker", "1h");
    expect(out).toEqual({ name: "e1", token: "tk", command: "cmd" });
    expect(JSON.parse(fetchMock.mock.calls[0][1].body)).toEqual({ name: "e1", backend: "docker", ttl: "1h" });
  });

  it("proxyEnabled returns the boolean flag", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ enabled: true }));
    expect(await new Api("t").proxyEnabled()).toBe(true);
  });

  it("createBox maps the session envelope to box_id/token", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ session: { BoxID: "b1", Token: "tok" } }));
    const out = await new Api("t").createBox("b1", "desc", "edge-1");
    expect(out).toEqual({ box_id: "b1", token: "tok" });
    expect(JSON.parse(fetchMock.mock.calls[0][1].body)).toEqual({
      opts: { BoxID: "b1", Description: "desc", SpokeName: "edge-1" },
    });
  });

  it("authPageURL returns the url", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ url: "https://hub/auth/tok" }));
    expect(await new Api("t").authPageURL("tok")).toBe("https://hub/auth/tok");
  });

  it("createProxy returns the proxy and posts the fields", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ proxy: { box_id: "b", port: 80, url: "u", slug: "s" } }));
    const out = await new Api("t").createProxy("b", 80, "d");
    expect(out.url).toBe("u");
    expect(JSON.parse(fetchMock.mock.calls[0][1].body)).toEqual({ box_id: "b", port: 80, description: "d" });
  });

  it("deleteProxy / dropSpoke / setDefaultSpoke / revokeJoinToken send their keys", async () => {
    fetchMock.mockResolvedValue(jsonResponse({}));
    const api = new Api("t");
    await api.deleteProxy("b", 80);
    expect(JSON.parse(fetchMock.mock.calls[0][1].body)).toEqual({ box_id: "b", port: 80 });
    await api.dropSpoke("e1");
    expect(JSON.parse(fetchMock.mock.calls[1][1].body)).toEqual({ name: "e1" });
    await api.setDefaultSpoke("e1");
    expect(JSON.parse(fetchMock.mock.calls[2][1].body)).toEqual({ name: "e1" });
    await api.revokeJoinToken("id1");
    expect(JSON.parse(fetchMock.mock.calls[3][1].body)).toEqual({ id: "id1" });
  });
});
