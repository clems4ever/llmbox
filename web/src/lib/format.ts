// Small presentation helpers shared across the dashboard: turning API strings
// into the short, human forms the UI shows. They are pure so they can be unit
// tested directly, keeping formatting logic out of the components.
import type { BoxView } from "../api";

/** shortTime trims an ISO timestamp to "YYYY-MM-DD HH:MM", or "" when empty —
 * the compact form the tables show for enrollment/expiry times.
 *
 * @arg iso An ISO-8601 timestamp, possibly empty or undefined.
 * @return string The "YYYY-MM-DD HH:MM" prefix, or "" when there is no input.
 */
export function shortTime(iso?: string): string {
  if (!iso) return "";
  return iso.slice(0, 16).replace("T", " ");
}

/** isExpired reports whether an ISO expiry timestamp is in the past.
 *
 * @arg iso An ISO-8601 timestamp.
 * @return boolean True when the instant has already elapsed.
 */
export function isExpired(iso: string): boolean {
  return new Date(iso).getTime() < Date.now();
}

/** createdAt renders a box's numeric created field (Unix seconds) as a short
 * local timestamp, or "" when the box carries no creation time.
 *
 * @arg created The box's created field, in Unix seconds (0 when unknown).
 * @return string The "YYYY-MM-DD HH:MM" local time, or "" when created is 0.
 */
export function createdAt(created: number): string {
  if (!created) return "";
  const d = new Date(created * 1000);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

/** boxId returns the stable identifier the UI uses for a workspace: its box_id
 * when set, else its display name, else its instance ID (a record observed
 * before its first sync may carry no name yet) — the same value the
 * destroy/proxy calls key on.
 *
 * @arg b The box to identify.
 * @return string The box_id, falling back to name, then instance_id.
 */
export function boxId(b: BoxView): string {
  return b.box_id || b.name || b.instance_id;
}

/** isBroken reports whether a box's phase marks it broken — its init script
 * failed during provisioning, so it never came up. "broken" is the only
 * non-empty phase the box lifecycle produces; a healthy box omits the field
 * entirely (the API drops the empty phase), so phase may be undefined.
 *
 * @arg phase The box's phase string, or undefined for a healthy box.
 * @return boolean True when the box is broken.
 */
export function isBroken(phase?: string): boolean {
  return (phase ?? "").toLowerCase() === "broken";
}

export type StateTone = "running" | "unreachable" | "terminated" | "paused" | "stopped";

/** stateTone classifies a box state for its badge colour: running is healthy,
 * unreachable means the box's spoke is offline (the box itself may be fine),
 * terminated is a tombstone for a box confirmed gone, paused is a box
 * deliberately stopped to save compute (resumable), and anything else (exited,
 * …) is a stopped-ish backend state.
 *
 * @arg state The box's state string, or undefined.
 * @return StateTone The tone the state badge should use.
 */
export function stateTone(state?: string): StateTone {
  const s = (state ?? "").toLowerCase();
  if (s === "running") return "running";
  if (s === "unreachable") return "unreachable";
  if (s === "terminated") return "terminated";
  if (s === "paused") return "paused";
  return "stopped";
}

/** lastSeenAt renders a box's last_seen field (Unix seconds) as a short local
 * timestamp, or "" when the hub never observed the box.
 *
 * @arg lastSeen The box's last_seen field, in Unix seconds (0 or undefined when unknown).
 * @return string The "YYYY-MM-DD HH:MM" local time, or "" when unknown.
 */
export function lastSeenAt(lastSeen?: number): string {
  return createdAt(lastSeen ?? 0);
}

/** formatBytes renders a byte count as a short human string (B/kB/MB/GB/TB,
 * base-1000), e.g. 1420 -> "1.4 kB". It is used by the network-audit table to
 * keep per-flow byte totals compact.
 *
 * @arg n A non-negative byte count.
 * @return string The compact human-readable size.
 */
export function formatBytes(n: number): string {
  if (!n || n < 0) return "0 B";
  const units = ["B", "kB", "MB", "GB", "TB"];
  let i = 0;
  let v = n;
  while (v >= 1000 && i < units.length - 1) {
    v /= 1000;
    i += 1;
  }
  // Whole bytes show no decimal; larger units show one.
  return `${i === 0 ? v : v.toFixed(1)} ${units[i]}`;
}

/** clockTime renders a Unix-seconds timestamp as a local "HH:MM:SS" clock, or ""
 * when unset — the compact "when" the live network-audit table shows per flow.
 *
 * @arg secs A Unix timestamp in seconds (0/undefined when unknown).
 * @return string The "HH:MM:SS" local time, or "" when unknown.
 */
export function clockTime(secs?: number): string {
  if (!secs) return "";
  const d = new Date(secs * 1000);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}
