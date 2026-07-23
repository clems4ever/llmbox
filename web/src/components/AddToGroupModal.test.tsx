import { describe, expect, it, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { AddToGroupModal } from "./AddToGroupModal";
import { render, mockApi, allowlistGroup } from "../test/utils";

describe("AddToGroupModal", () => {
  it("adds the domain to an existing group", async () => {
    const add = vi.fn().mockResolvedValue(allowlistGroup());
    const onDone = vi.fn().mockResolvedValue(undefined);
    const groups = [allowlistGroup({ id: "gh", name: "gh" })];
    const { user } = render(
      <AddToGroupModal
        api={mockApi({ addDomainToGroup: add })}
        domain="registry.npmjs.org"
        groups={groups}
        opened
        onClose={vi.fn()}
        onDone={onDone}
      />,
    );
    // "gh" is preselected (first group); add & allow.
    await user.click(screen.getByRole("button", { name: /Add & allow/ }));
    await waitFor(() =>
      expect(add).toHaveBeenCalledWith("registry.npmjs.org", { groupId: "gh" }),
    );
    expect(onDone).toHaveBeenCalled();
  });

  it("creates a new group when none is chosen", async () => {
    const add = vi.fn().mockResolvedValue(allowlistGroup());
    const { user } = render(
      <AddToGroupModal
        api={mockApi({ addDomainToGroup: add })}
        domain="registry.npmjs.org"
        groups={[]}
        opened
        onClose={vi.fn()}
        onDone={vi.fn()}
      />,
    );
    await user.type(screen.getByRole("textbox", { name: "New group name" }), "node-pkgs");
    await user.click(screen.getByRole("button", { name: /Add & allow/ }));
    await waitFor(() =>
      expect(add).toHaveBeenCalledWith("registry.npmjs.org", { newGroupName: "node-pkgs" }),
    );
  });
});
