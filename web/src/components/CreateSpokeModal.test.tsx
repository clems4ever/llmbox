import { describe, expect, it, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { CreateSpokeModal } from "./CreateSpokeModal";
import { render, mockApi } from "../test/utils";

describe("CreateSpokeModal", () => {
  it("renders nothing when closed", () => {
    render(<CreateSpokeModal api={mockApi()} opened={false} onClose={vi.fn()} refresh={vi.fn()} />);
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("enrolls a spoke and shows the one-time command", async () => {
    const api = mockApi({
      createSpoke: vi.fn().mockResolvedValue({ name: "edge-9", token: "tk", command: "llmbox-spoke join --token tk" }),
    });
    const refresh = vi.fn().mockResolvedValue(undefined);
    const { user } = render(
      <CreateSpokeModal api={api} opened onClose={vi.fn()} refresh={refresh} />,
    );
    await user.type(screen.getByPlaceholderText("edge-1"), "edge-9");
    await user.click(screen.getByRole("button", { name: "Create runner" }));

    await waitFor(() => expect(api.createSpoke).toHaveBeenCalledWith("edge-9", "docker", ""));
    expect(await screen.findByText("llmbox-spoke join --token tk")).toBeInTheDocument();
    expect(refresh).toHaveBeenCalled();
  });

  it("offers the systemd setup script in a second tab", async () => {
    const api = mockApi({
      createSpoke: vi.fn().mockResolvedValue({
        name: "edge-9",
        token: "tk",
        command: "llmbox-spoke docker --hub wss://hub/spoke/connect --token tk --state llmbox-spoke.json",
      }),
    });
    const { user } = render(
      <CreateSpokeModal api={api} opened onClose={vi.fn()} refresh={vi.fn()} />,
    );
    await user.type(screen.getByPlaceholderText("edge-1"), "edge-9");
    await user.click(screen.getByRole("button", { name: "Create runner" }));

    await user.click(await screen.findByRole("tab", { name: "systemd service" }));
    expect(
      await screen.findByText(/sudo systemctl enable --now llmbox-spoke\.service/),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Copy script" })).toBeInTheDocument();
  });

  it("closes via Cancel", async () => {
    const onClose = vi.fn();
    const { user } = render(<CreateSpokeModal api={mockApi()} opened onClose={onClose} refresh={vi.fn()} />);
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    expect(onClose).toHaveBeenCalled();
  });
});
