// pty holds the browser end of the host→box pty-control protocol the in-browser
// terminal speaks to a box's guest (see internal/guest/pty.go). Each message is
// [type:1][len:4 big-endian][payload], letting keystrokes and window resizes share
// the single WebSocket. The box→browser direction is the shell's raw output and
// needs no framing here.

/** ptyMsgData tags a message carrying raw terminal input (keystrokes/paste). */
const ptyMsgData = 0;
/** ptyMsgResize tags a message carrying a new terminal size (cols, rows). */
const ptyMsgResize = 1;

/** encodePTYControl frames one pty-control message: a type byte, a 4-byte
 * big-endian length, then the payload.
 *
 * @arg msgType The message type byte.
 * @arg payload The message payload.
 * @return Uint8Array The framed message ready to send as a binary WebSocket frame.
 */
function encodePTYControl(msgType: number, payload: Uint8Array): Uint8Array {
  const out = new Uint8Array(5 + payload.length);
  out[0] = msgType;
  new DataView(out.buffer).setUint32(1, payload.length, false);
  out.set(payload, 5);
  return out;
}

/** encodePTYInput frames terminal input (a keystroke or pasted text) for the box.
 *
 * @arg data The UTF-8 encoded input bytes.
 * @return Uint8Array The framed data message.
 */
export function encodePTYInput(data: Uint8Array): Uint8Array {
  return encodePTYControl(ptyMsgData, data);
}

/** encodePTYResize frames a window-size change for the box's PTY.
 *
 * @arg cols The terminal width in columns.
 * @arg rows The terminal height in rows.
 * @return Uint8Array The framed resize message.
 */
export function encodePTYResize(cols: number, rows: number): Uint8Array {
  const payload = new Uint8Array(4);
  const view = new DataView(payload.buffer);
  view.setUint16(0, cols, false);
  view.setUint16(2, rows, false);
  return encodePTYControl(ptyMsgResize, payload);
}

/** terminalWebSocketURL builds the same-origin WebSocket URL for a box's terminal
 * endpoint, carrying the initial terminal size. It derives ws/wss from the page's
 * protocol so it works behind TLS. The login cookie rides the handshake
 * automatically, which is how the endpoint authenticates the browser.
 *
 * @arg boxId The box to open a terminal into.
 * @arg cols The initial terminal width in columns.
 * @arg rows The initial terminal height in rows.
 * @return string The absolute ws(s):// URL for the terminal.
 */
export function terminalWebSocketURL(boxId: string, cols: number, rows: number): string {
  const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
  const path = `/api/v1/boxes/${encodeURIComponent(boxId)}/terminal?cols=${cols}&rows=${rows}`;
  return `${proto}//${window.location.host}${path}`;
}
