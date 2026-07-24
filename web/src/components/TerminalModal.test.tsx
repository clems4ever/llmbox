import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, waitFor } from "@testing-library/react";
import { render, box } from "../test/utils";

// A fake xterm Terminal that records what it is asked to write and exposes the
// onData/onResize callbacks so the spec can drive input and geometry without a
// real canvas (jsdom has none).
const termState = {
  onData: undefined as ((d: string) => void) | undefined,
  onResize: undefined as ((s: { cols: number; rows: number }) => void) | undefined,
  writes: [] as Array<Uint8Array | string>,
  disposed: false,
  cols: 100,
  rows: 30,
};

vi.mock("@xterm/xterm", () => ({
  Terminal: class {
    cols = termState.cols;
    rows = termState.rows;
    loadAddon() {}
    open() {}
    focus() {}
    write(d: Uint8Array | string) {
      termState.writes.push(d);
    }
    onData(cb: (d: string) => void) {
      termState.onData = cb;
      return { dispose() {} };
    }
    onResize(cb: (s: { cols: number; rows: number }) => void) {
      termState.onResize = cb;
      return { dispose() {} };
    }
    dispose() {
      termState.disposed = true;
    }
  },
}));

vi.mock("@xterm/addon-fit", () => ({
  FitAddon: class {
    fit() {}
  },
}));

// A controllable WebSocket stand-in: it records the URL and every frame sent, and
// lets the spec fire lifecycle events.
class MockWebSocket {
  static OPEN = 1;
  static instances: MockWebSocket[] = [];
  static CLOSED = 3;
  url: string;
  binaryType = "blob";
  readyState = MockWebSocket.OPEN;
  sent: Uint8Array[] = [];
  onopen: (() => void) | null = null;
  onmessage: ((ev: MessageEvent) => void) | null = null;
  onclose: (() => void) | null = null;
  onerror: (() => void) | null = null;
  closed = false;
  constructor(url: string) {
    this.url = url;
    MockWebSocket.instances.push(this);
  }
  send(data: Uint8Array) {
    this.sent.push(data);
  }
  close() {
    this.closed = true;
    this.readyState = MockWebSocket.CLOSED;
  }
}

describe("TerminalModal", () => {
  beforeEach(() => {
    termState.onData = undefined;
    termState.onResize = undefined;
    termState.writes = [];
    termState.disposed = false;
    MockWebSocket.instances = [];
    vi.stubGlobal("WebSocket", MockWebSocket);
  });
  afterEach(() => vi.unstubAllGlobals());

  async function open() {
    const { TerminalModal } = await import("./TerminalModal");
    return render(<TerminalModal box={box({ box_id: "term-box" })} opened onClose={() => {}} />);
  }

  it("opens a WebSocket to the box terminal endpoint with the fitted size", async () => {
    await open();
    await waitFor(() => expect(MockWebSocket.instances.length).toBe(1));
    expect(MockWebSocket.instances[0].url).toContain("/api/v1/boxes/term-box/terminal?cols=100&rows=30");
  });

  it("streams keystrokes as framed input and sends a resize on open", async () => {
    await open();
    await waitFor(() => expect(MockWebSocket.instances.length).toBe(1));
    const ws = MockWebSocket.instances[0];

    act(() => ws.onopen?.());
    // The open handler sends an initial resize frame (type byte 1).
    expect(ws.sent.some((f) => f[0] === 1)).toBe(true);

    act(() => termState.onData?.("hi"));
    // The keystroke is framed as a data message (type byte 0) carrying "hi".
    const dataFrame = ws.sent.find((f) => f[0] === 0);
    expect(dataFrame).toBeDefined();
    expect(Array.from(dataFrame!.slice(5))).toEqual([0x68, 0x69]);
  });

  it("writes received binary output to the terminal", async () => {
    await open();
    await waitFor(() => expect(MockWebSocket.instances.length).toBe(1));
    const ws = MockWebSocket.instances[0];

    const payload = new Uint8Array([0x6f, 0x6b]); // "ok"
    act(() => ws.onmessage?.({ data: payload.buffer } as MessageEvent));
    expect(termState.writes.length).toBe(1);
    expect(Array.from(termState.writes[0] as Uint8Array)).toEqual([0x6f, 0x6b]);
  });

  it("shows the connection state as a header status light", async () => {
    // The Modal header renders in a portal, so query the whole document.
    await open();
    await waitFor(() => expect(MockWebSocket.instances.length).toBe(1));
    // Before the socket opens, the light reflects the connecting state.
    expect(document.querySelector(`[data-terminal-state="connecting"]`)).not.toBeNull();
    const ws = MockWebSocket.instances[0];
    act(() => ws.onopen?.());
    expect(document.querySelector(`[data-terminal-state="connected"]`)).not.toBeNull();
    act(() => ws.onclose?.());
    expect(document.querySelector(`[data-terminal-state="closed"]`)).not.toBeNull();
  });

  it("closes the socket and disposes the terminal on unmount", async () => {
    const { unmount } = await open();
    await waitFor(() => expect(MockWebSocket.instances.length).toBe(1));
    const ws = MockWebSocket.instances[0];
    act(() => unmount());
    expect(ws.closed).toBe(true);
    expect(termState.disposed).toBe(true);
  });
});
