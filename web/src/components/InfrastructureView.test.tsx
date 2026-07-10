import { describe, expect, it, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import { InfrastructureView } from "./InfrastructureView";
import { render, mockApi, dashboardData, spoke, token } from "../test/utils";

describe("InfrastructureView", () => {
  it("renders a skeleton while data is null", () => {
    const { container } = render(<InfrastructureView api={mockApi()} data={null} refresh={vi.fn()} />);
    expect(container.querySelector(".mantine-Skeleton-root")).toBeTruthy();
  });

  it("shows the empty runners hint", () => {
    render(<InfrastructureView api={mockApi()} data={dashboardData()} refresh={vi.fn()} />);
    expect(screen.getByText(/No runners enrolled yet/i)).toBeInTheDocument();
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
    expect(await screen.findByText("default runner is now edge-2")).toBeInTheDocument();
  });

  it("confirms and drops a spoke", async () => {
    const api = mockApi();
    const refresh = vi.fn().mockResolvedValue(undefined);
    const data = dashboardData({ spokes: [spoke({ name: "edge-2" })] });
    const { user } = render(<InfrastructureView api={api} data={data} refresh={refresh} />);
    await user.click(screen.getByRole("button", { name: "Drop edge-2" }));
    const dialog = await screen.findByRole("dialog", { name: "Drop runner" });
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

  it("re-shows setup instructions for a token with a placeholder and notice", async () => {
    const data = dashboardData({
      spokes: [spoke()],
      tokens: [token({ name: "edge-1" })],
    });
    const { user } = render(<InfrastructureView api={mockApi()} data={data} refresh={vi.fn()} />);
    await user.click(screen.getByRole("button", { name: "Setup instructions for edge-1" }));

    const dialog = await screen.findByRole("dialog", { name: "Runner setup — edge-1" });
    // The command is re-rendered with the placeholder, never the secret...
    expect(within(dialog).getByText(/--token <one-time-token>/)).toBeInTheDocument();
    // ...and the notice explains the token was shown only at creation.
    expect(
      within(dialog).getByText(/shown only when the runner was created/),
    ).toBeInTheDocument();
    // The systemd tab is offered here too.
    await user.click(within(dialog).getByRole("tab", { name: "systemd service" }));
    expect(
      await within(dialog).findByText(/sudo systemctl enable --now llmbox-spoke\.service/),
    ).toBeInTheDocument();
  });

  it("regenerates a lost token and shows the fresh command once", async () => {
    const api = mockApi();
    const refresh = vi.fn().mockResolvedValue(undefined);
    const data = dashboardData({
      spokes: [spoke()],
      tokens: [token({ id: "tid-1", name: "edge-1" })],
    });
    const { user } = render(<InfrastructureView api={api} data={data} refresh={refresh} />);
    await user.click(screen.getByRole("button", { name: "Setup instructions for edge-1" }));

    const dialog = await screen.findByRole("dialog", { name: "Runner setup — edge-1" });
    await user.click(within(dialog).getByRole("button", { name: "Regenerate token" }));

    await waitFor(() => expect(api.regenerateJoinToken).toHaveBeenCalledWith("tid-1"));
    // The fresh real command replaces the placeholder one...
    expect(await within(dialog).findByText(/--token fresh-token/)).toBeInTheDocument();
    expect(within(dialog).queryByText(/--token <one-time-token>/)).not.toBeInTheDocument();
    // ...the lost-token notice is gone, a shown-once reminder appears, and the
    // token list is refreshed (the old ID no longer exists).
    expect(within(dialog).queryByText(/shown only when the runner was created/)).not.toBeInTheDocument();
    expect(within(dialog).getByText(/save it this time/)).toBeInTheDocument();
    expect(refresh).toHaveBeenCalled();
  });

  it("opens the create-runner modal", async () => {
    const { user } = render(<InfrastructureView api={mockApi()} data={dashboardData()} refresh={vi.fn()} />);
    await user.click(screen.getByRole("button", { name: "New runner" }));
    expect(await screen.findByRole("dialog", { name: "New runner" })).toBeInTheDocument();
  });
});
