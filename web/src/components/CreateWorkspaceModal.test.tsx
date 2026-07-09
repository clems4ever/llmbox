import { describe, expect, it, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { CreateWorkspaceModal } from "./CreateWorkspaceModal";
import { render, mockApi, spoke } from "../test/utils";

describe("CreateWorkspaceModal", () => {
  it("renders nothing when closed", () => {
    render(
      <CreateWorkspaceModal api={mockApi()} spokes={[]} opened={false} onClose={vi.fn()} refresh={vi.fn()} />,
    );
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("lists connected spokes plus the default option", async () => {
    const spokes = [spoke({ name: "edge-1", connected: true, default: true }), spoke({ name: "edge-2", connected: false })];
    render(
      <CreateWorkspaceModal api={mockApi()} spokes={spokes} opened onClose={vi.fn()} refresh={vi.fn()} />,
    );
    // The default option names the current default spoke.
    expect(screen.getByDisplayValue("default (edge-1)")).toBeInTheDocument();
  });

  it("creates a workspace and shows its activation link", async () => {
    const api = mockApi({
      createBox: vi.fn().mockResolvedValue({ box_id: "myws", token: "tok" }),
      authPageURL: vi.fn().mockResolvedValue("https://hub/auth/tok"),
    });
    const refresh = vi.fn().mockResolvedValue(undefined);
    const { user } = render(
      <CreateWorkspaceModal api={api} spokes={[]} opened onClose={vi.fn()} refresh={refresh} />,
    );
    await user.type(screen.getByPlaceholderText("refactor-auth"), "myws");
    await user.click(screen.getByRole("button", { name: "Create workspace" }));

    await waitFor(() => expect(api.createBox).toHaveBeenCalledWith("myws", "", ""));
    expect(api.authPageURL).toHaveBeenCalledWith("tok");
    expect(await screen.findByDisplayValue("https://hub/auth/tok")).toBeInTheDocument();
    expect(refresh).toHaveBeenCalled();
  });

  it("closes via Cancel", async () => {
    const onClose = vi.fn();
    const { user } = render(
      <CreateWorkspaceModal api={mockApi()} spokes={[]} opened onClose={onClose} refresh={vi.fn()} />,
    );
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    expect(onClose).toHaveBeenCalled();
  });
});
