import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { Dashboard } from "./Dashboard";
import { ApiError } from "../api";
import { redirectToSignIn } from "../lib/navigation";
import { render, mockApi, box, spoke } from "../test/utils";

vi.mock("../lib/navigation", () => ({ redirectToSignIn: vi.fn() }));
const redirect = vi.mocked(redirectToSignIn);

const session = { email: "admin@b.c", admin: true, csrf: "x" };

beforeEach(() => redirect.mockReset());
afterEach(() => vi.clearAllMocks());

describe("Dashboard", () => {
  it("loads and shows workspaces by default", async () => {
    const api = mockApi({ listBoxes: vi.fn().mockResolvedValue([box({ box_id: "alpha" })]) });
    render(<Dashboard api={api} session={session} />);
    expect(await screen.findByText("alpha")).toBeInTheDocument();
  });

  it("switches to the infrastructure view", async () => {
    const api = mockApi({ spokeStatuses: vi.fn().mockResolvedValue([spoke({ name: "edge-1" })]) });
    const { user } = render(<Dashboard api={api} session={session} />);
    await screen.findAllByText("Workspaces");
    await user.click(screen.getByText("Infrastructure"));
    expect(await screen.findByText("Runners")).toBeInTheDocument();
    expect(screen.getByText("edge-1")).toBeInTheDocument();
  });

  it("opens the details drawer when a workspace is selected", async () => {
    const api = mockApi({ listBoxes: vi.fn().mockResolvedValue([box({ box_id: "alpha" })]) });
    const { user } = render(<Dashboard api={api} session={session} />);
    await user.click(await screen.findByText("alpha"));
    expect(await screen.findByText("HTTP proxies")).toBeInTheDocument();
  });

  it("reloads the data when Refresh is clicked", async () => {
    const listBoxes = vi.fn().mockResolvedValue([box({ box_id: "alpha" })]);
    const api = mockApi({ listBoxes });
    const { user } = render(<Dashboard api={api} session={session} />);
    await screen.findByText("alpha");
    await user.click(screen.getByRole("button", { name: "Refresh" }));
    await waitFor(() => expect(listBoxes).toHaveBeenCalledTimes(2));
  });

  it("bounces to sign-in when a refresh returns 401", async () => {
    const api = mockApi({ listBoxes: vi.fn().mockRejectedValue(new ApiError(401, "expired")) });
    render(<Dashboard api={api} session={session} />);
    await waitFor(() => expect(redirect).toHaveBeenCalled());
  });

  it("shows the signed-in email in the sidebar footer", async () => {
    const api = mockApi();
    render(<Dashboard api={api} session={session} />);
    expect(await screen.findByText("admin@b.c")).toBeInTheDocument();
  });

  it("signs out and bounces to sign-in", async () => {
    const logout = vi.fn().mockResolvedValue({});
    const api = mockApi({ logout });
    const { user } = render(<Dashboard api={api} session={session} />);
    await screen.findByText("admin@b.c");
    await user.click(screen.getByRole("button", { name: "Sign out" }));
    await waitFor(() => expect(redirect).toHaveBeenCalled());
    expect(logout).toHaveBeenCalled();
  });

  it("still bounces to sign-in when logout answers 401 (session already gone)", async () => {
    const api = mockApi({ logout: vi.fn().mockRejectedValue(new ApiError(401, "not signed in")) });
    const { user } = render(<Dashboard api={api} session={session} />);
    await screen.findByText("admin@b.c");
    await user.click(screen.getByRole("button", { name: "Sign out" }));
    await waitFor(() => expect(redirect).toHaveBeenCalled());
  });

  it("stays and reports the error when logout fails", async () => {
    const api = mockApi({ logout: vi.fn().mockRejectedValue(new ApiError(500, "boom")) });
    const { user } = render(<Dashboard api={api} session={session} />);
    await screen.findByText("admin@b.c");
    await user.click(screen.getByRole("button", { name: "Sign out" }));
    expect(await screen.findByText("Couldn't sign out")).toBeInTheDocument();
    expect(redirect).not.toHaveBeenCalled();
  });
});
