import { describe, expect, it, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import { InfrastructureView } from "./InfrastructureView";
import { render, mockApi, dashboardData, spoke, token } from "../test/utils";

describe("InfrastructureView", () => {
  it("renders a skeleton while data is null", () => {
    const { container } = render(<InfrastructureView api={mockApi()} data={null} refresh={vi.fn()} />);
    expect(container.querySelector(".mantine-Skeleton-root")).toBeTruthy();
  });

  it("shows the empty spokes hint", () => {
    render(<InfrastructureView api={mockApi()} data={dashboardData()} refresh={vi.fn()} />);
    expect(screen.getByText(/No spokes enrolled yet/i)).toBeInTheDocument();
  });

  it("renders spoke status and the default marker", () => {
    const data = dashboardData({
      spokes: [spoke({ name: "edge-1", connected: true, default: true }), spoke({ name: "edge-2", connected: false })],
    });
    render(<InfrastructureView api={mockApi()} data={data} refresh={vi.fn()} />);
    expect(screen.getByText("connected")).toBeInTheDocument();
    expect(screen.getByText("offline")).toBeInTheDocument();
    expect(screen.getByText("default")).toBeInTheDocument();
  });

  it("makes a spoke the default", async () => {
    const api = mockApi();
    const refresh = vi.fn().mockResolvedValue(undefined);
    const data = dashboardData({ spokes: [spoke({ name: "edge-2", connected: true })] });
    const { user } = render(<InfrastructureView api={api} data={data} refresh={refresh} />);
    await user.click(screen.getByRole("button", { name: "Make edge-2 default" }));
    await waitFor(() => expect(api.setDefaultSpoke).toHaveBeenCalledWith("edge-2"));
    expect(await screen.findByText("default spoke is now edge-2")).toBeInTheDocument();
  });

  it("confirms and drops a spoke", async () => {
    const api = mockApi();
    const refresh = vi.fn().mockResolvedValue(undefined);
    const data = dashboardData({ spokes: [spoke({ name: "edge-2" })] });
    const { user } = render(<InfrastructureView api={api} data={data} refresh={refresh} />);
    await user.click(screen.getByRole("button", { name: "Drop edge-2" }));
    const dialog = await screen.findByRole("dialog", { name: "Drop spoke" });
    await user.click(within(dialog).getByRole("button", { name: "Drop" }));
    await waitFor(() => expect(api.dropSpoke).toHaveBeenCalledWith("edge-2"));
  });

  it("lists outstanding join tokens and revokes one", async () => {
    const api = mockApi();
    const refresh = vi.fn().mockResolvedValue(undefined);
    const data = dashboardData({
      spokes: [spoke()],
      tokens: [token({ id: "abcdef012345xyz", name: "edge-1", expires_at: "2000-01-01T00:00:00Z" })],
    });
    const { user } = render(<InfrastructureView api={api} data={data} refresh={refresh} />);
    expect(screen.getByText("abcdef012345")).toBeInTheDocument(); // truncated to 12 chars
    expect(screen.getByText("expired")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Revoke token" }));
    const dialog = await screen.findByRole("dialog", { name: "Revoke join token" });
    await user.click(within(dialog).getByRole("button", { name: "Revoke" }));
    await waitFor(() => expect(api.revokeJoinToken).toHaveBeenCalledWith("abcdef012345xyz"));
  });

  it("opens the create-spoke modal", async () => {
    const { user } = render(<InfrastructureView api={mockApi()} data={dashboardData()} refresh={vi.fn()} />);
    await user.click(screen.getByRole("button", { name: "New spoke" }));
    expect(await screen.findByRole("dialog", { name: "New spoke" })).toBeInTheDocument();
  });
});
