import { describe, expect, it } from "vitest";
import { screen } from "@testing-library/react";
import { StateBadge, StatusBadge } from "./StatusBadge";
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

describe("StateBadge", () => {
  it("shows a plain badge for a running box", () => {
    render(<StateBadge state="running" />);
    const badge = screen.getByText("running");
    expect(badge).toBeInTheDocument();
    expect(badge.closest("[data-box-state]")).toHaveAttribute("data-box-state", "running");
  });

  it("keeps an unreachable box visible with its state", () => {
    render(<StateBadge state="unreachable" lastSeen={1750000000} />);
    expect(screen.getByText("unreachable")).toBeInTheDocument();
  });

  it("renders a terminated tombstone", () => {
    render(<StateBadge state="terminated" />);
    expect(screen.getByText("terminated")).toBeInTheDocument();
  });
});
