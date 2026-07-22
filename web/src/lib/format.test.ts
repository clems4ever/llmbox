import { describe, expect, it, vi } from "vitest";
import { boxId, clockTime, createdAt, formatBytes, isBroken, isExpired, shortTime, stateTone } from "./format";

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

describe("formatBytes", () => {
  it("shows whole bytes without a decimal", () => {
    expect(formatBytes(0)).toBe("0 B");
    expect(formatBytes(512)).toBe("512 B");
  });
  it("scales to kB/MB/GB with one decimal (base 1000)", () => {
    expect(formatBytes(1420)).toBe("1.4 kB");
    expect(formatBytes(5300)).toBe("5.3 kB");
    expect(formatBytes(2_500_000)).toBe("2.5 MB");
    expect(formatBytes(3_000_000_000)).toBe("3.0 GB");
  });
  it("treats missing/negative counts as zero", () => {
    expect(formatBytes(-5)).toBe("0 B");
  });
});

describe("clockTime", () => {
  it("renders a HH:MM:SS local clock", () => {
    // 1_700_000_050 is a fixed instant; assert the shape rather than a zone.
    expect(clockTime(1_700_000_050)).toMatch(/^\d{2}:\d{2}:\d{2}$/);
  });
  it("returns empty for an unset timestamp", () => {
    expect(clockTime(0)).toBe("");
    expect(clockTime(undefined)).toBe("");
  });
});
