import { describe, expect, it, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import { WorkspacesView } from "./WorkspacesView";
import { render, mockApi, box, dashboardData, spoke } from "../test/utils";

describe("WorkspacesView", () => {
  it("shows the empty state and opens the create modal", async () => {
    const { user } = render(
      <WorkspacesView api={mockApi()} data={dashboardData()} refresh={vi.fn()} onSelect={vi.fn()} />,
    );
    expect(screen.getByText("No workspaces yet")).toBeInTheDocument();
    // Both the header and the empty state offer "New workspace"; click the first.
    await user.click(screen.getAllByRole("button", { name: "New workspace" })[0]);
    expect(await screen.findByRole("dialog")).toBeInTheDocument();
    expect(screen.getByPlaceholderText("refactor-auth")).toBeInTheDocument();
  });

  it("renders a skeleton while data is null", () => {
    const { container } = render(
      <WorkspacesView api={mockApi()} data={null} refresh={vi.fn()} onSelect={vi.fn()} />,
    );
    expect(container.querySelector(".mantine-Skeleton-root")).toBeTruthy();
  });

  it("renders the table view and selects a workspace on row click", async () => {
    const onSelect = vi.fn();
    const data = dashboardData({
      boxes: [box({ box_id: "alpha", description: "the alpha box", spoke: "edge-1" })],
    });
    const { user } = render(
      <WorkspacesView api={mockApi()} data={data} refresh={vi.fn()} onSelect={onSelect} />,
    );
    expect(screen.getByText("alpha")).toBeInTheDocument();
    expect(screen.getByText("the alpha box")).toBeInTheDocument();
    await user.click(screen.getByText("alpha"));
    expect(onSelect).toHaveBeenCalledWith("alpha");
  });

  it("switches to the grid view", async () => {
    const data = dashboardData({ boxes: [box({ box_id: "alpha" })] });
    const { user } = render(
      <WorkspacesView api={mockApi()} data={data} refresh={vi.fn()} onSelect={vi.fn()} />,
    );
    await user.click(screen.getByRole("radio", { name: "Grid view" }));
    // The grid container keeps the same id and still lists the box.
    expect(screen.getByText("alpha")).toBeInTheDocument();
  });

  it("marks a broken box with a broken badge", () => {
    const data = dashboardData({ boxes: [box({ box_id: "alpha", phase: "broken" })] });
    render(<WorkspacesView api={mockApi()} data={data} refresh={vi.fn()} onSelect={vi.fn()} />);
    expect(screen.getByText("broken")).toBeInTheDocument();
  });

  it("confirms and removes a workspace", async () => {
    const api = mockApi();
    const refresh = vi.fn().mockResolvedValue(undefined);
    const data = dashboardData({ boxes: [box({ box_id: "alpha" })], spokes: [spoke()] });
    const { user } = render(
      <WorkspacesView api={api} data={data} refresh={refresh} onSelect={vi.fn()} />,
    );
    await user.click(screen.getByRole("button", { name: "Remove alpha" }));
    const dialog = await screen.findByRole("dialog");
    await user.click(within(dialog).getByRole("button", { name: "Remove" }));
    await waitFor(() => expect(api.destroyBox).toHaveBeenCalledWith("alpha"));
    await waitFor(() => expect(refresh).toHaveBeenCalled());
    expect(await screen.findByText("removed workspace alpha")).toBeInTheDocument();
  });

  it("pauses a running workspace without a confirm dialog", async () => {
    const api = mockApi();
    const refresh = vi.fn().mockResolvedValue(undefined);
    const data = dashboardData({ boxes: [box({ box_id: "alpha", state: "running" })] });
    const { user } = render(
      <WorkspacesView api={api} data={data} refresh={refresh} onSelect={vi.fn()} />,
    );
    await user.click(screen.getByRole("button", { name: "Pause alpha" }));
    await waitFor(() => expect(api.pauseBox).toHaveBeenCalledWith("alpha"));
    await waitFor(() => expect(refresh).toHaveBeenCalled());
    expect(await screen.findByText("paused workspace alpha")).toBeInTheDocument();
  });

  it("shows Resume (not Pause) for a paused workspace and resumes it", async () => {
    const api = mockApi();
    const refresh = vi.fn().mockResolvedValue(undefined);
    const data = dashboardData({ boxes: [box({ box_id: "alpha", state: "paused" })] });
    const { user } = render(
      <WorkspacesView api={api} data={data} refresh={refresh} onSelect={vi.fn()} />,
    );
    expect(screen.queryByRole("button", { name: "Pause alpha" })).toBeNull();
    await user.click(screen.getByRole("button", { name: "Resume alpha" }));
    await waitFor(() => expect(api.resumeBox).toHaveBeenCalledWith("alpha"));
    await waitFor(() => expect(refresh).toHaveBeenCalled());
    expect(await screen.findByText("resumed workspace alpha")).toBeInTheDocument();
  });

  it("shows Start (not Pause) for a stopped workspace and resumes it", async () => {
    const api = mockApi();
    const refresh = vi.fn().mockResolvedValue(undefined);
    // A stopped box (dead VMM, e.g. after a host reboot) reports "stopped". Use a
    // distinct id so its success toast doesn't collide with the paused case's.
    const data = dashboardData({ boxes: [box({ box_id: "gamma", state: "stopped" })] });
    const { user } = render(
      <WorkspacesView api={api} data={data} refresh={refresh} onSelect={vi.fn()} />,
    );
    expect(screen.queryByRole("button", { name: "Pause gamma" })).toBeNull();
    await user.click(screen.getByRole("button", { name: "Start gamma" }));
    await waitFor(() => expect(api.resumeBox).toHaveBeenCalledWith("gamma"));
    await waitFor(() => expect(refresh).toHaveBeenCalled());
    expect(await screen.findByText("resumed workspace gamma")).toBeInTheDocument();
  });

  it("shows Start for an exited (docker) workspace too", () => {
    // Docker reports a stopped box as "exited"; it maps to the same stopped tone.
    const data = dashboardData({ boxes: [box({ box_id: "alpha", state: "exited" })] });
    render(<WorkspacesView api={mockApi()} data={data} refresh={vi.fn()} onSelect={vi.fn()} />);
    expect(screen.getByRole("button", { name: "Start alpha" })).toBeInTheDocument();
  });

  it("offers neither Pause nor Resume/Start for an unreachable workspace", () => {
    const data = dashboardData({ boxes: [box({ box_id: "alpha", state: "unreachable" })] });
    render(<WorkspacesView api={mockApi()} data={data} refresh={vi.fn()} onSelect={vi.fn()} />);
    expect(screen.queryByRole("button", { name: "Pause alpha" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Resume alpha" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Start alpha" })).toBeNull();
  });

  it("offers no Start button for a terminated workspace", () => {
    // Terminated is a tombstone — the box is gone from its spoke, so resume-box
    // would fail; the UI must not offer to start it.
    const data = dashboardData({ boxes: [box({ box_id: "alpha", state: "terminated" })] });
    render(<WorkspacesView api={mockApi()} data={data} refresh={vi.fn()} onSelect={vi.fn()} />);
    expect(screen.queryByRole("button", { name: "Start alpha" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Resume alpha" })).toBeNull();
  });
});
