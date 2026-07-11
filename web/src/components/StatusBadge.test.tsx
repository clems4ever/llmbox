import { describe, expect, it } from "vitest";
import { screen } from "@testing-library/react";
import { StateBadge, StatusBadge } from "./StatusBadge";
import { render } from "../test/utils";

describe("StatusBadge", () => {
  it("shows a broken badge for a broken box", () => {
    render(<StatusBadge phase="broken" />);
    expect(screen.getByText("broken")).toBeInTheDocument();
  });

  it("renders no badge for a healthy box (empty phase)", () => {
    render(<StatusBadge phase="" />);
    expect(screen.queryByText("broken")).not.toBeInTheDocument();
    expect(document.querySelector(".mantine-Badge-root")).toBeNull();
  });

  it("renders no badge (and does not crash) when phase is absent", () => {
    // A healthy box omits phase entirely, so the prop is undefined; this must not
    // throw (the real API drops the empty phase field).
    render(<StatusBadge />);
    expect(screen.queryByText("broken")).not.toBeInTheDocument();
    expect(document.querySelector(".mantine-Badge-root")).toBeNull();
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
