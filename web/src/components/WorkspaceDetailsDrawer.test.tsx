import { describe, expect, it, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import { WorkspaceDetailsDrawer } from "./WorkspaceDetailsDrawer";
import { render, mockApi, box, proxy, flow } from "../test/utils";

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

  describe("network activity", () => {
    it("fetches and lists the box's audited flows", async () => {
      const api = mockApi({
        boxNetwork: vi.fn().mockResolvedValue([
          flow({ dst_ip: "140.82.121.4", dst_port: 443, bytes_out: 1420, bytes_in: 5300 }),
          flow({ proto: "udp", dst_ip: "8.8.8.8", dst_port: 53, src_port: 34567, bytes_out: 60, bytes_in: 200, state: "" }),
        ]),
      });
      renderDrawer({ api });
      await waitFor(() => expect(api.boxNetwork).toHaveBeenCalledWith("alpha"));
      expect(await screen.findByText("Network activity")).toBeInTheDocument();
      expect(screen.getByText("140.82.121.4:443")).toBeInTheDocument();
      expect(screen.getByText("8.8.8.8:53")).toBeInTheDocument();
      // Byte totals are humanised.
      expect(screen.getByText("5.3 kB")).toBeInTheDocument();
      // Flow count badge reflects the number of flows.
      expect(screen.getByText("2 flows")).toBeInTheDocument();
    });

    it("shows an empty hint when a box has no recorded flows", async () => {
      const api = mockApi({ boxNetwork: vi.fn().mockResolvedValue([]) });
      renderDrawer({ api });
      expect(
        await screen.findByText("No outbound connections recorded for this workspace yet."),
      ).toBeInTheDocument();
    });

    it("surfaces a network fetch error", async () => {
      const api = mockApi({ boxNetwork: vi.fn().mockRejectedValue(new Error("spoke offline")) });
      renderDrawer({ api });
      expect(await screen.findByText(/Could not load network activity: spoke offline/)).toBeInTheDocument();
    });
  });
});
