import { describe, expect, it, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { GroupEditorModal } from "./GroupEditorModal";
import { render, mockApi, allowlistGroup } from "../test/utils";

describe("GroupEditorModal", () => {
  it("renders nothing when closed", () => {
    render(<GroupEditorModal api={mockApi()} group={null} opened={false} onClose={vi.fn()} onSaved={vi.fn()} />);
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("creates a new group from the form", async () => {
    const save = vi.fn().mockResolvedValue(allowlistGroup());
    const onSaved = vi.fn().mockResolvedValue(undefined);
    const onClose = vi.fn();
    const { user } = render(
      <GroupEditorModal api={mockApi({ saveAllowlistGroup: save })} group={null} opened onClose={onClose} onSaved={onSaved} />,
    );
    await user.type(screen.getByRole("textbox", { name: "Name" }), "github");
    // TagsInput accepts a domain on Enter.
    await user.type(screen.getByPlaceholderText("add a domain…"), "github.com{enter}");
    await user.click(screen.getByRole("button", { name: "Create group" }));

    await waitFor(() =>
      expect(save).toHaveBeenCalledWith(
        expect.objectContaining({ name: "github", is_global: false, domains: ["github.com"], ttl_seconds: 30 }),
      ),
    );
    expect(onSaved).toHaveBeenCalled();
    expect(onClose).toHaveBeenCalled();
  });

  it("pre-fills the form and preserves the id when editing", async () => {
    const save = vi.fn().mockResolvedValue(allowlistGroup());
    const group = allowlistGroup({ id: "gh", name: "gh", is_global: true, domains: ["github.com"], ttl_seconds: 60 });
    const { user } = render(
      <GroupEditorModal api={mockApi({ saveAllowlistGroup: save })} group={group} opened onClose={vi.fn()} onSaved={vi.fn()} />,
    );
    expect(screen.getByRole("textbox", { name: "Name" })).toHaveValue("gh");
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() =>
      expect(save).toHaveBeenCalledWith(expect.objectContaining({ id: "gh", is_global: true })),
    );
  });
});
