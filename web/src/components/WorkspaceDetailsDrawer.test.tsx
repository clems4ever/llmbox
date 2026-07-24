import { describe, expect, it, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import { WorkspaceDetailsDrawer } from "./WorkspaceDetailsDrawer";
import { render, mockApi, box, proxy } from "../test/utils";

function renderDrawer(props: Partial<Parameters<typeof WorkspaceDetailsDrawer>[0]> = {}) {
  return render(
    <WorkspaceDetailsDrawer
      api={props.api ?? mockApi()}
      box={props.box === undefined ? box({ box_id: "alpha", spoke: "edge-1" }) : props.box}
      proxyEnabled={props.proxyEnabled ?? true}
      proxies={props.proxies ?? []}
      refresh={props.refresh ?? vi.fn()}
      onClose={props.onClose ?? vi.fn()}
    />,
  );
}

describe("WorkspaceDetailsDrawer", () => {
  it("renders nothing when no box is selected", () => {
    renderDrawer({ box: null });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("shows the workspace metadata", () => {
    renderDrawer({ box: box({ box_id: "alpha", spoke: "edge-1", image: "img:1", description: "hi there" }) });
    expect(screen.getByText("hi there")).toBeInTheDocument();
    expect(screen.getByText("edge-1")).toBeInTheDocument();
    expect(screen.getByText("img:1")).toBeInTheDocument();
  });

  it("offers an Open terminal action for a running box", () => {
    renderDrawer({ box: box({ box_id: "alpha", state: "running" }) });
    expect(screen.getByTestId("open-terminal")).toBeEnabled();
  });

  it("renders without crashing when the allowlist has null groups", async () => {
    // The API marshals an empty group set as null; the drawer must tolerate it.
    const api = mockApi({
      getBoxAllowlist: vi.fn().mockResolvedValue({
        box_id: "alpha",
        group_ids: null,
        effective_groups: null,
        effective_domains: null,
      }),
    });
    renderDrawer({ api, box: box({ box_id: "alpha", state: "running" }) });
    await waitFor(() => expect(screen.getByText(/Egress deny-by-default/i)).toBeInTheDocument());
    expect(screen.getByTestId("open-terminal")).toBeInTheDocument();
  });

  it("disables the terminal for a terminated box", () => {
    renderDrawer({ box: box({ box_id: "alpha", state: "terminated" }) });
    expect(screen.getByTestId("open-terminal")).toBeDisabled();
  });

  it("shows a note when the proxy feature is disabled", () => {
    renderDrawer({ proxyEnabled: false });
    expect(screen.getByText(/reverse proxy is not enabled/i)).toBeInTheDocument();
    expect(screen.queryByText("HTTP proxies")).not.toBeInTheDocument();
  });

  it("lists this workspace's proxies", () => {
    renderDrawer({ proxies: [proxy({ box_id: "alpha", port: 8080, url: "https://alpha.hub/" })] });
    expect(screen.getByText("HTTP proxies")).toBeInTheDocument();
    expect(screen.getByText("8080")).toBeInTheDocument();
    expect(screen.getByText("https://alpha.hub/").closest("a")).toHaveAttribute("href", "https://alpha.hub/");
  });

  it("shows the empty proxies hint", () => {
    renderDrawer({ proxies: [] });
    expect(screen.getByText("No proxies for this workspace yet.")).toBeInTheDocument();
  });

  it("pings each proxy and shows an up badge when it serves", async () => {
    const api = mockApi({ pingProxy: vi.fn().mockResolvedValue({ ok: true, status: 200, latency_ms: 5 }) });
    renderDrawer({ api, proxies: [proxy({ box_id: "alpha", port: 8080 })] });
    await waitFor(() => expect(api.pingProxy).toHaveBeenCalledWith("alpha", 8080));
    expect(await screen.findByText("up")).toBeInTheDocument();
  });

  it("shows a down badge when the proxy port does not answer", async () => {
    const api = mockApi({
      pingProxy: vi.fn().mockResolvedValue({ ok: false, error: "the box is not reachable on this port" }),
    });
    renderDrawer({ api, proxies: [proxy({ box_id: "alpha", port: 8080 })] });
    expect(await screen.findByText("down")).toBeInTheDocument();
  });

  it("re-checks a proxy when its status badge is clicked", async () => {
    const api = mockApi({ pingProxy: vi.fn().mockResolvedValue({ ok: true, status: 200 }) });
    const { user } = renderDrawer({ api, proxies: [proxy({ box_id: "alpha", port: 8080 })] });
    const badge = await screen.findByText("up");
    (api.pingProxy as ReturnType<typeof vi.fn>).mockClear();
    await user.click(badge);
    await waitFor(() => expect(api.pingProxy).toHaveBeenCalledWith("alpha", 8080));
  });

  it("adds a proxy", async () => {
    const api = mockApi();
    const refresh = vi.fn().mockResolvedValue(undefined);
    const { user } = renderDrawer({ api, refresh });
    await user.type(screen.getByPlaceholderText("8000"), "9000");
    await user.click(screen.getByRole("button", { name: "Add proxy" }));
    await waitFor(() => expect(api.createProxy).toHaveBeenCalledWith("alpha", 9000, ""));
    expect(refresh).toHaveBeenCalled();
  });

  it("confirms and deletes a proxy", async () => {
    const api = mockApi();
    const refresh = vi.fn().mockResolvedValue(undefined);
    const { user } = renderDrawer({
      api,
      refresh,
      proxies: [proxy({ box_id: "alpha", port: 8080 })],
    });
    await user.click(screen.getByRole("button", { name: "Remove proxy 8080" }));
    const dialog = await screen.findByRole("dialog", { name: "Remove proxy" });
    await user.click(within(dialog).getByRole("button", { name: "Remove" }));
    await waitFor(() => expect(api.deleteProxy).toHaveBeenCalledWith("alpha", 8080));
    expect(refresh).toHaveBeenCalled();
  });

  it("shows the init-script output for a broken box", () => {
    renderDrawer({
      box: box({
        box_id: "alpha",
        phase: "broken",
        last_error: "init script failed: exit status 9\n\nboom-in-init",
      }),
    });
    expect(screen.getByText("Init script failed")).toBeInTheDocument();
    expect(screen.getByText(/boom-in-init/)).toBeInTheDocument();
  });

  it("does not show the init-script panel for a healthy box", () => {
    renderDrawer({ box: box({ box_id: "alpha", phase: "" }) });
    expect(screen.queryByText("Init script failed")).not.toBeInTheDocument();
  });
});
