import { describe, expect, it, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { BoxGroupsModal } from "./BoxGroupsModal";
import { render, mockApi, allowlistGroup, box } from "../test/utils";

describe("BoxGroupsModal", () => {
  const groups = [
    allowlistGroup({ id: "core", name: "core", is_global: true }),
    allowlistGroup({ id: "gh", name: "gh", is_global: false }),
    allowlistGroup({ id: "pypi", name: "pypi", is_global: false }),
  ];

  it("pre-checks the box's assigned groups and saves the new set", async () => {
    const getBoxAllowlist = vi.fn().mockResolvedValue({
      box_id: "web",
      group_ids: ["gh"],
      effective_groups: ["core", "gh"],
      effective_domains: [],
    });
    const setBoxGroups = vi.fn().mockResolvedValue({});
    const onSaved = vi.fn().mockResolvedValue(undefined);
    const { user } = render(
      <BoxGroupsModal
        api={mockApi({ getBoxAllowlist, setBoxGroups })}
        box={box({ box_id: "web" })}
        groups={groups}
        opened
        onClose={vi.fn()}
        onSaved={onSaved}
      />,
    );
    // The pre-assigned "gh" checkbox is checked; add "pypi".
    const gh = await screen.findByRole("checkbox", { name: "gh" });
    expect(gh).toBeChecked();
    await user.click(screen.getByRole("checkbox", { name: "pypi" }));
    await user.click(screen.getByRole("button", { name: "Save groups" }));

    await waitFor(() => expect(setBoxGroups).toHaveBeenCalledWith("web", ["gh", "pypi"]));
    expect(onSaved).toHaveBeenCalled();
  });

  it("shows global groups as checked and disabled", async () => {
    const getBoxAllowlist = vi
      .fn()
      .mockResolvedValue({ box_id: "web", group_ids: [], effective_groups: ["core"], effective_domains: [] });
    render(
      <BoxGroupsModal
        api={mockApi({ getBoxAllowlist })}
        box={box({ box_id: "web" })}
        groups={groups}
        opened
        onClose={vi.fn()}
        onSaved={vi.fn()}
      />,
    );
    const core = await screen.findByRole("checkbox", { name: /core \(global\)/ });
    expect(core).toBeChecked();
    expect(core).toBeDisabled();
  });
});
