import { describe, it, expect } from "vitest";
import { encodePTYInput, encodePTYResize, terminalWebSocketURL } from "./pty";

describe("pty framing", () => {
  it("frames input as [type=0][len:4 BE][payload]", () => {
    const frame = encodePTYInput(new Uint8Array([0x68, 0x69])); // "hi"
    expect(Array.from(frame)).toEqual([0, 0, 0, 0, 2, 0x68, 0x69]);
  });

  it("frames an empty input with a zero length", () => {
    const frame = encodePTYInput(new Uint8Array([]));
    expect(Array.from(frame)).toEqual([0, 0, 0, 0, 0]);
  });

  it("frames a resize as [type=1][len=4][cols:2 BE][rows:2 BE]", () => {
    const frame = encodePTYResize(132, 50);
    expect(Array.from(frame)).toEqual([1, 0, 0, 0, 4, 0, 132, 0, 50]);
  });

  it("encodes a wide column count big-endian", () => {
    const frame = encodePTYResize(300, 24); // 300 = 0x012C
    expect(Array.from(frame)).toEqual([1, 0, 0, 0, 4, 0x01, 0x2c, 0, 24]);
  });
});

describe("terminalWebSocketURL", () => {
  it("builds a same-origin ws URL carrying the size", () => {
    const url = terminalWebSocketURL("my-box", 100, 30);
    expect(url).toBe(`ws://${window.location.host}/api/v1/boxes/my-box/terminal?cols=100&rows=30`);
  });

  it("escapes the box id", () => {
    const url = terminalWebSocketURL("a b/c", 80, 24);
    expect(url).toContain("/api/v1/boxes/a%20b%2Fc/terminal");
  });
});
