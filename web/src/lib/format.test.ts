import { describe, expect, it, vi } from "vitest";
import { boxId, createdAt, isBroken, isExpired, shortTime, stateTone } from "./format";

describe("shortTime", () => {
  it("trims an ISO timestamp to minutes and drops the T", () => {
    expect(shortTime("2026-01-02T03:04:05Z")).toBe("2026-01-02 03:04");
  });
  it("returns empty string for empty or missing input", () => {
    expect(shortTime("")).toBe("");
    expect(shortTime(undefined)).toBe("");
  });
});

describe("isExpired", () => {
  it("is true for a past instant and false for a future one", () => {
    expect(isExpired("2000-01-01T00:00:00Z")).toBe(true);
    expect(isExpired("2999-01-01T00:00:00Z")).toBe(false);
  });
});

describe("createdAt", () => {
  it("renders Unix seconds as a local YYYY-MM-DD HH:MM string", () => {
    vi.useFakeTimers();
    // 2021-01-01T00:00:00Z; assert via a fresh Date so the local tz matches.
    const secs = Math.floor(Date.UTC(2021, 0, 1, 12, 30, 0) / 1000);
    const out = createdAt(secs);
    expect(out).toMatch(/^2021-01-01 \d{2}:\d{2}$/);
    vi.useRealTimers();
  });
  it("returns empty string when created is 0", () => {
    expect(createdAt(0)).toBe("");
  });
});

describe("boxId", () => {
  it("prefers box_id and falls back to name", () => {
    expect(boxId({ box_id: "a", name: "b" } as never)).toBe("a");
    expect(boxId({ box_id: "", name: "b" } as never)).toBe("b");
    expect(boxId({ name: "b" } as never)).toBe("b");
  });
});

describe("isBroken", () => {
  it("is true only for a broken phase, case-insensitively", () => {
    expect(isBroken("broken")).toBe(true);
    expect(isBroken("Broken")).toBe(true);
  });
  it("is false for a healthy (empty) phase", () => {
    expect(isBroken("")).toBe(false);
  });
  it("is false (and does not throw) when phase is undefined", () => {
    // Healthy boxes omit phase, so the API sends undefined.
    expect(isBroken(undefined)).toBe(false);
  });
});

describe("stateTone", () => {
  it("classifies the known lifecycle states", () => {
    expect(stateTone("running")).toBe("running");
    expect(stateTone("unreachable")).toBe("unreachable");
    expect(stateTone("terminated")).toBe("terminated");
  });
  it("gives paused its own tone (distinct from other stopped states)", () => {
    expect(stateTone("paused")).toBe("paused");
    expect(stateTone("Paused")).toBe("paused");
  });
  it("maps other backend states to stopped", () => {
    expect(stateTone("exited")).toBe("stopped");
    expect(stateTone("")).toBe("stopped");
  });
  it("does not throw on an undefined state", () => {
    expect(stateTone(undefined)).toBe("stopped");
  });
});
