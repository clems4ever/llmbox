import { describe, expect, it, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import { NetworkView } from "./NetworkView";
import { render, mockApi, allowlistGroup, box, dashboardData } from "../test/utils";

describe("NetworkView", () => {
  it("lists the allowlist groups with their domain counts", async () => {
    const api = mockApi({
      listAllowlistGroups: vi.fn().mockResolvedValue([
        allowlistGroup({ id: "core-ai", name: "core-ai", is_global: true }),
        allowlistGroup({ id: "github", name: "github", domains: ["github.com"], box_count: 3 }),
      ]),
    });
    render(<NetworkView api={api} data={dashboardData()} />);
    expect(await screen.findByText("core-ai")).toBeInTheDocument();
    expect(screen.getByText("github")).toBeInTheDocument();
    // The global group renders an "All workspaces" usage line.
    expect(screen.getByText("All workspaces")).toBeInTheDocument();
  });

  it("opens the editor from New group", async () => {
    const { user } = render(<NetworkView api={mockApi()} data={dashboardData()} />);
    await user.click(screen.getByRole("button", { name: "New group" }));
    expect(await screen.findByRole("dialog", { name: "New allowlist group" })).toBeInTheDocument();
  });

  it("shows an empty state when there are no groups", async () => {
    render(<NetworkView api={mockApi({ listAllowlistGroups: vi.fn().mockResolvedValue([]) })} data={dashboardData()} />);
    expect(await screen.findByText("No allowlist groups yet")).toBeInTheDocument();
  });

  it("toggles a group global from the Assignments tab", async () => {
    const save = vi.fn().mockResolvedValue({});
    const api = mockApi({
      listAllowlistGroups: vi.fn().mockResolvedValue([allowlistGroup({ id: "gh", name: "gh", is_global: false })]),
      saveAllowlistGroup: save,
      getBoxAllowlist: vi
        .fn()
        .mockResolvedValue({ box_id: "box-1", group_ids: [], effective_groups: [], effective_domains: [] }),
    });
    const { user } = render(<NetworkView api={api} data={dashboardData({ boxes: [box()] })} />);
    await user.click(await screen.findByRole("tab", { name: "Assignments" }));
    const row = (await screen.findByText("gh")).closest("div")!;
    await user.click(within(row.parentElement as HTMLElement).getByRole("switch"));
    await waitFor(() => expect(save).toHaveBeenCalledWith(expect.objectContaining({ id: "gh", is_global: true })));
  });
});
