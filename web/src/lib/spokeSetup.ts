// Builders for the runner setup instructions: turning the server's one-line
// enrollment command into the systemd installation script offered beside it.
// The command's shape is owned by the server (spokeRunCommand); this module
// only adapts it for a service context — absolute binary path and a persistent
// state location — so the unit survives reboots and working-directory changes.

/** spokeServiceName is the systemd unit the setup script installs. */
export const spokeServiceName = "llmbox-spoke.service";

/** tokenPlaceholder mirrors the server's stand-in for the join-token secret in
 * commands re-rendered after creation (api.TokenPlaceholder). */
export const tokenPlaceholder = "<one-time-token>";

/** systemdSetupScript builds the copy-pasteable script that installs the spoke
 * as a systemd service and starts it now and on boot. It rewrites the server's
 * enrollment command for service use: the bare binary name becomes the absolute
 * /usr/local/bin path (systemd does not search $PATH) and the state file is
 * pinned to /var/lib/llmbox — the spoke's built-in default lives under the
 * user's home, which is the wrong place for a system service. A firecracker
 * spoke additionally pins --state-dir to /var/lib/llmbox/firecracker so its
 * multi-GiB guest images and per-box state land on disk, not the default
 * in-memory tmpfs run-dir (too small, and wiped on reboot). Keeping --token in
 * the unit is safe: the spoke only uses it on first enrollment and reconnects
 * from the saved credential afterwards. The unit runs as root (the systemd system
 * default) — required because a firecracker spoke launches every microVM through
 * the jailer, which must chroot, create device nodes, and drop to a per-VM UID. A
 * firecracker spoke also sets KillMode=process so systemd signals only the spoke
 * process on stop/restart, leaving its jailed microVMs running (each is detached
 * into its own session and jailer cgroup, and rehydrated when the spoke respawns) —
 * the default control-group kill would reap every VM, mirroring how Docker's own
 * daemon unit uses KillMode=process to keep containers alive across a restart. */
export function systemdSetupScript(command: string): string {
  const firecracker = /^llmbox-spoke\s+firecracker\b/.test(command);
  const exec =
    command
      .replace(/^llmbox-spoke\s+/, "/usr/local/bin/llmbox-spoke ")
      .replace(/\s+--state\s+\S+/, "")
      .replace(/\s+--state-dir\s+\S+/, "") +
    " --state /var/lib/llmbox/llmbox-spoke.json" +
    (firecracker ? " --state-dir /var/lib/llmbox/firecracker" : "");
  return `sudo tee /etc/systemd/system/${spokeServiceName} >/dev/null <<'UNIT'
[Unit]
Description=llmbox spoke runner
Wants=network-online.target
After=network-online.target

[Service]
ExecStart=${exec}
Restart=on-failure
RestartSec=5
StateDirectory=llmbox${firecracker ? "\nKillMode=process" : ""}

[Install]
WantedBy=multi-user.target
UNIT

sudo systemctl daemon-reload
sudo systemctl enable --now ${spokeServiceName}`;
}
