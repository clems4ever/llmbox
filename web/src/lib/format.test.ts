import { describe, expect, it, vi } from "vitest";
import { boxId, createdAt, isExpired, phaseTone, shortTime } from "./format";

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

describe("phaseTone", () => {
  it("maps ready/running to ready", () => {
    expect(phaseTone("ready")).toBe("ready");
    expect(phaseTone("Running")).toBe("ready");
  });
  it("maps error/fail phases to error", () => {
    expect(phaseTone("error")).toBe("error");
    expect(phaseTone("provision-failed")).toBe("error");
  });
  it("maps anything else to pending", () => {
    expect(phaseTone("pending")).toBe("pending");
    expect(phaseTone("")).toBe("pending");
  });
});
