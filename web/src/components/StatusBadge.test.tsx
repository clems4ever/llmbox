import { describe, expect, it } from "vitest";
import { screen } from "@testing-library/react";
import { StatusBadge } from "./StatusBadge";
import { render } from "../test/utils";

describe("StatusBadge", () => {
  it("shows the phase text", () => {
    render(<StatusBadge phase="ready" />);
    expect(screen.getByText("ready")).toBeInTheDocument();
  });

  it("falls back to 'unknown' for an empty phase", () => {
    render(<StatusBadge phase="" />);
    expect(screen.getByText("unknown")).toBeInTheDocument();
  });
});
