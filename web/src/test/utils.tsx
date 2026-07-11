// Shared test helpers: a render() that wraps a component in the same provider
// stack main.tsx mounts (theme, modals, notifications), and factories for the
// mock Api and sample data the component specs drive. Keeping these in one place
// means a spec only declares the data that matters to it.
import type { ReactElement } from "react";
import { render as rtlRender, type RenderResult } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MantineProvider } from "@mantine/core";
import { ModalsProvider } from "@mantine/modals";
import { Notifications } from "@mantine/notifications";
import { vi } from "vitest";
import { theme } from "../theme";
import type { Api, BoxView, JoinTokenInfo, ProxyInfo, SpokeStatus } from "../api";
import type { DashboardData } from "../lib/data";

/** render wraps a component in the app's provider stack, so specs exercise it
 * exactly as it renders in production. */
export function render(ui: ReactElement): RenderResult & { user: ReturnType<typeof userEvent.setup> } {
  const user = userEvent.setup();
  const result = rtlRender(
    <MantineProvider theme={theme} defaultColorScheme="light">
      <ModalsProvider>
        <Notifications />
        {ui}
      </ModalsProvider>
    </MantineProvider>,
  );
  return { ...result, user };
}

/** mockApi returns a fake Api whose methods are vi.fn()s resolving to empty
 * results, overridable per spec. Cast to Api so components accept it. */
export function mockApi(overrides: Partial<Record<keyof Api, unknown>> = {}): Api {
  const base = {
    listBoxes: vi.fn().mockResolvedValue([]),
    spokeStatuses: vi.fn().mockResolvedValue([]),
    listJoinTokens: vi.fn().mockResolvedValue([]),
    proxyEnabled: vi.fn().mockResolvedValue(true),
    listProxies: vi.fn().mockResolvedValue([]),
    createSpoke: vi.fn().mockResolvedValue({ name: "edge-1", token: "t", command: "run me" }),
    dropSpoke: vi.fn().mockResolvedValue({}),
    setDefaultSpoke: vi.fn().mockResolvedValue({}),
    revokeJoinToken: vi.fn().mockResolvedValue({}),
    regenerateJoinToken: vi.fn().mockResolvedValue({
      name: "edge-1",
      token: "fresh-token",
      command: "llmbox-spoke docker --hub wss://hub/spoke/connect --token fresh-token",
    }),
    createBox: vi.fn().mockResolvedValue({ box_id: "box-1", token: "tok" }),
    authPageURL: vi.fn().mockResolvedValue("https://hub/auth/tok"),
    destroyBox: vi.fn().mockResolvedValue({}),
    pauseBox: vi.fn().mockResolvedValue({}),
    resumeBox: vi.fn().mockResolvedValue({}),
    createProxy: vi.fn().mockResolvedValue({ box_id: "box-1", port: 8000, url: "https://p", slug: "s" }),
    deleteProxy: vi.fn().mockResolvedValue({}),
    logout: vi.fn().mockResolvedValue({}),
  };
  return { ...base, ...overrides } as unknown as Api;
}

/** box builds a BoxView with sensible defaults, overridable per field. */
export function box(overrides: Partial<BoxView> = {}): BoxView {
  return {
    instance_id: "i-1",
    name: "box-1",
    box_id: "box-1",
    image: "llmbox-box:latest",
    state: "running",
    status: "Up",
    phase: "ready",
    created: 1_700_000_000,
    ...overrides,
  };
}

/** spoke builds a SpokeStatus with defaults. */
export function spoke(overrides: Partial<SpokeStatus> = {}): SpokeStatus {
  return { name: "edge-1", connected: true, enrolled_at: "2026-01-02T03:04:05Z", ...overrides };
}

/** token builds a JoinTokenInfo with defaults (a far-future expiry, and the
 * placeholder command the server re-renders for outstanding tokens). */
export function token(overrides: Partial<JoinTokenInfo> = {}): JoinTokenInfo {
  return {
    id: "abcdef012345",
    name: "edge-1",
    backend: "docker",
    command: "llmbox-spoke docker --hub wss://hub/spoke/connect --token <one-time-token>",
    expires_at: "2099-01-01T00:00:00Z",
    ...overrides,
  };
}

/** proxy builds a ProxyInfo with defaults. */
export function proxy(overrides: Partial<ProxyInfo> = {}): ProxyInfo {
  return { box_id: "box-1", port: 8000, url: "https://box-1.hub/", slug: "box-1-8000", ...overrides };
}

/** dashboardData assembles a DashboardData from the pieces, defaulting each to
 * empty and proxies to enabled. */
export function dashboardData(overrides: Partial<DashboardData> = {}): DashboardData {
  return {
    spokes: [],
    tokens: [],
    boxes: [],
    proxyEnabled: true,
    proxies: [],
    ...overrides,
  };
}
