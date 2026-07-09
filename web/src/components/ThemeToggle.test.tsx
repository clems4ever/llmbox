import { describe, expect, it } from "vitest";
import { screen } from "@testing-library/react";
import { ThemeToggle } from "./ThemeToggle";
import { render } from "../test/utils";

describe("ThemeToggle", () => {
  it("toggles the color scheme on click", async () => {
    const { user } = render(<ThemeToggle />);
    const btn = screen.getByRole("button", { name: /switch to dark theme/i });
    await user.click(btn);
    expect(screen.getByRole("button", { name: /switch to light theme/i })).toBeInTheDocument();
  });
});
