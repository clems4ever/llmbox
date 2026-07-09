import { describe, expect, it, vi } from "vitest";
import { loadDashboard, proxiesFor } from "./data";
import { mockApi, proxy } from "../test/utils";

describe("loadDashboard", () => {
  it("assembles every section and fetches proxies when enabled", async () => {
    const api = mockApi({
      spokeStatuses: async () => [{ name: "s", connected: true }],
      listBoxes: async () => [{ instance_id: "i", name: "b", image: "", state: "", status: "", phase: "", created: 0 }],
      proxyEnabled: async () => true,
      listProxies: vi.fn().mockResolvedValue([proxy()]),
    });
    const data = await loadDashboard(api);
    expect(data.spokes).toHaveLength(1);
    expect(data.boxes).toHaveLength(1);
    expect(data.proxyEnabled).toBe(true);
    expect(data.proxies).toHaveLength(1);
    expect(api.listProxies).toHaveBeenCalledOnce();
  });

  it("skips the proxy fetch when the feature is disabled", async () => {
    const api = mockApi({ proxyEnabled: async () => false });
    const data = await loadDashboard(api);
    expect(data.proxyEnabled).toBe(false);
    expect(data.proxies).toEqual([]);
    expect(api.listProxies).not.toHaveBeenCalled();
  });
});

describe("proxiesFor", () => {
  it("keeps only proxies for the given box", () => {
    const list = [proxy({ box_id: "a", port: 1 }), proxy({ box_id: "b", port: 2 }), proxy({ box_id: "a", port: 3 })];
    expect(proxiesFor(list, "a")).toHaveLength(2);
    expect(proxiesFor(list, "b")).toHaveLength(1);
    expect(proxiesFor(list, "c")).toHaveLength(0);
  });
});
