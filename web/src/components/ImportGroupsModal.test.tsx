import { describe, expect, it, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { ImportGroupsModal } from "./ImportGroupsModal";
import { render, mockApi } from "../test/utils";

describe("ImportGroupsModal", () => {
  it("parses a bundle and imports it", async () => {
    const importFn = vi.fn().mockResolvedValue(2);
    const onDone = vi.fn().mockResolvedValue(undefined);
    const onClose = vi.fn();
    const { user } = render(
      <ImportGroupsModal api={mockApi({ importAllowlistGroups: importFn })} opened onClose={onClose} onDone={onDone} />,
    );
    const bundle = '{"version":1,"groups":[{"name":"x","domains":["x.com"]}]}';
    // paste (not type) so JSON braces aren't parsed as userEvent special keys.
    await user.click(screen.getByRole("textbox", { name: "Bundle (JSON)" }));
    await user.paste(bundle);
    await user.click(screen.getByRole("button", { name: "Import" }));

    await waitFor(() =>
      expect(importFn).toHaveBeenCalledWith(
        { version: 1, groups: [{ name: "x", domains: ["x.com"] }] },
        "merge",
      ),
    );
    expect(onDone).toHaveBeenCalled();
    expect(onClose).toHaveBeenCalled();
  });

  it("rejects invalid JSON without calling the api", async () => {
    const importFn = vi.fn();
    const { user } = render(
      <ImportGroupsModal api={mockApi({ importAllowlistGroups: importFn })} opened onClose={vi.fn()} onDone={vi.fn()} />,
    );
    await user.type(screen.getByRole("textbox", { name: "Bundle (JSON)" }), "not json");
    await user.click(screen.getByRole("button", { name: "Import" }));

    expect(await screen.findByText(/isn't valid JSON/)).toBeInTheDocument();
    expect(importFn).not.toHaveBeenCalled();
  });
});
