// TerminalModal is the in-browser terminal into a box: it opens a WebSocket to the
// box's PTY-backed terminal endpoint and renders it with xterm.js. Keystrokes are
// framed and streamed to the box's shell; the shell's raw output is written back
// to the terminal. The terminal fits its container and streams resizes so the
// remote PTY tracks the visible geometry — including when the modal goes
// full-screen on phones and tablets.
import { useEffect, useRef, useState } from "react";
import { Badge, Group, Modal, Text } from "@mantine/core";
import { useMediaQuery } from "@mantine/hooks";
import { IconTerminal2 } from "@tabler/icons-react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import "@fontsource/jetbrains-mono/400.css";
import "@fontsource/jetbrains-mono/700.css";
import type { BoxView } from "../api";
import { boxId as boxIdOf } from "../lib/format";
import { encodePTYInput, encodePTYResize, terminalWebSocketURL } from "../lib/pty";

export interface TerminalModalProps {
  box: BoxView | null;
  opened: boolean;
  onClose: () => void;
}

/** ConnState is the terminal's connection lifecycle, surfaced as a status badge. */
type ConnState = "connecting" | "connected" | "closed";

/** terminalBackground is the terminal surface colour; the host padding uses it too
 * so the breathing room around the text reads as part of the terminal. */
const terminalBackground = "#1a1614";

/** terminalFontFamily prefers JetBrains Mono (bundled via @fontsource) with a
 * monospace stack fallback, so glyphs are terminal-appropriate and aligned. */
const terminalFontFamily = '"JetBrains Mono", ui-monospace, SFMono-Regular, Menlo, Consolas, monospace';

/** fullScreenQuery matches phones and tablets (portrait and most landscape),
 * where the terminal should take the whole screen rather than a centered dialog. */
const fullScreenQuery = "(max-width: 1200px)";

/** TerminalModal renders a live shell into the selected box. It is a centered,
 * roomy dialog on the desktop and a full-screen surface on phones/tablets, where a
 * windowed terminal would be unusably small.
 *
 * @arg props The box to open a terminal into, and the modal open/close controls.
 * @return JSX.Element The terminal modal.
 */
export function TerminalModal({ box, opened, onClose }: TerminalModalProps): JSX.Element {
  const id = box ? boxIdOf(box) : "";
  const fullScreen = useMediaQuery(fullScreenQuery) ?? false;
  return (
    <Modal
      opened={opened && box !== null}
      onClose={onClose}
      fullScreen={fullScreen}
      size="80rem"
      padding="md"
      // In full-screen the body must fill the viewport below the header so the
      // terminal can stretch to the whole screen; otherwise it sizes to content.
      styles={fullScreen ? { body: { height: "calc(100dvh - 3.5rem)" } } : undefined}
      title={
        <Group gap="xs">
          <IconTerminal2 size={18} />
          <Text fw={600}>Terminal</Text>
          <Text c="dimmed" className="mono-wrap">{id}</Text>
        </Group>
      }
    >
      {opened && box && <TerminalView boxId={id} fullScreen={fullScreen} />}
    </Modal>
  );
}

/** TerminalView mounts xterm.js and bridges it to the box's terminal WebSocket. It
 * is a child so the terminal is created fresh each time the modal opens (and torn
 * down on close), keeping no stale socket or buffer between sessions. It fills the
 * available height and refits on any container or viewport change, so the terminal
 * always matches the screen — full-screen on mobile/tablet included.
 *
 * @arg props The box id to connect to and whether the modal is full-screen.
 * @return JSX.Element The terminal host element and a status line.
 */
function TerminalView({ boxId, fullScreen }: { boxId: string; fullScreen: boolean }): JSX.Element {
  const hostRef = useRef<HTMLDivElement>(null);
  const [state, setState] = useState<ConnState>("connecting");

  useEffect(() => {
    const host = hostRef.current;
    if (!host) return;
    let disposed = false;

    const term = new Terminal({
      cursorBlink: true,
      fontFamily: terminalFontFamily,
      fontSize: 13,
      lineHeight: 1.15,
      theme: { background: terminalBackground, foreground: "#f5efe9" },
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(host);

    const doFit = () => {
      if (disposed) return;
      try {
        fit.fit();
      } catch {
        // The host may be detached mid-teardown; a failed fit is harmless.
      }
    };
    doFit();
    // Web fonts load asynchronously; xterm measures glyphs at open, so refit once
    // JetBrains Mono is ready or the grid would be sized to the fallback metrics.
    if (typeof document !== "undefined" && document.fonts?.ready) {
      void document.fonts.ready.then(doFit);
    }

    const ws = new WebSocket(terminalWebSocketURL(boxId, term.cols, term.rows));
    ws.binaryType = "arraybuffer";
    const encoder = new TextEncoder();

    ws.onopen = () => {
      setState("connected");
      // The URL already carried the fitted size, but send one more resize so a
      // late layout settle (fonts, scrollbar) still reaches the PTY.
      ws.send(encodePTYResize(term.cols, term.rows));
      term.focus();
    };
    ws.onmessage = (ev: MessageEvent) => {
      if (ev.data instanceof ArrayBuffer) {
        term.write(new Uint8Array(ev.data));
      } else if (typeof ev.data === "string") {
        term.write(ev.data);
      }
    };
    ws.onclose = () => {
      setState("closed");
      term.write("\r\n\x1b[38;5;244m[connection closed]\x1b[0m\r\n");
    };
    ws.onerror = () => setState("closed");

    const dataSub = term.onData((d: string) => {
      if (ws.readyState === WebSocket.OPEN) ws.send(encodePTYInput(encoder.encode(d)));
    });
    const resizeSub = term.onResize(({ cols, rows }: { cols: number; rows: number }) => {
      if (ws.readyState === WebSocket.OPEN) ws.send(encodePTYResize(cols, rows));
    });

    // Refit whenever the host resizes (modal open/close animation, viewport or
    // orientation change, full-screen toggle) so the terminal always matches the
    // screen; onResize above streams the new size to the PTY.
    const observer = new ResizeObserver(doFit);
    observer.observe(host);

    return () => {
      disposed = true;
      observer.disconnect();
      dataSub.dispose();
      resizeSub.dispose();
      // Closing the socket ends the remote shell (the hub tears the PTY down when
      // the WebSocket goes), so no session lingers after the modal closes.
      ws.onclose = null;
      ws.close();
      term.dispose();
    };
  }, [boxId]);

  return (
    <div
      style={{
        display: "flex",
        flexDirection: "column",
        gap: 8,
        height: fullScreen ? "100%" : "60vh",
      }}
    >
      <div
        ref={hostRef}
        data-testid="terminal-host"
        style={{
          flex: 1,
          minHeight: 0,
          background: terminalBackground,
          borderRadius: 8,
          padding: 12,
          overflow: "hidden",
        }}
      />
      <Group justify="flex-end">
        <StatusBadgeFor state={state} />
      </Group>
    </div>
  );
}

/** StatusBadgeFor renders the terminal's connection state. */
function StatusBadgeFor({ state }: { state: ConnState }): JSX.Element {
  const map: Record<ConnState, { color: string; label: string }> = {
    connecting: { color: "yellow", label: "connecting…" },
    connected: { color: "green", label: "connected" },
    closed: { color: "gray", label: "disconnected" },
  };
  const { color, label } = map[state];
  return (
    <Badge color={color} variant="light" data-terminal-state={state}>
      {label}
    </Badge>
  );
}
