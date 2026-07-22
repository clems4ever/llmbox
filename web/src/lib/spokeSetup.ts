// Builders for the runner setup instructions: turning the server's one-line
// enrollment command into the systemd installation script offered beside it.
// The command's shape is owned by the server (spokeRunCommand); this module
// only adapts it for a service context — absolute binary path and a persistent
// state location — so the unit survives reboots and working-directory changes.

/** spokeServiceName is the systemd unit the setup script installs. */
export const spokeServiceName = "llmbox-spoke.service";

/** networkServiceName is the root-run oneshot the firecracker setup script
 * installs to provision the host TAP/NAT egress pool before the spoke starts, so
 * the long-running spoke can attach to it with --egress-mode=external instead of
 * mutating host networking itself. */
export const networkServiceName = "llmbox-firecracker-network.service";

/** tokenPlaceholder mirrors the server's stand-in for the join-token secret in
 * commands re-rendered after creation (api.TokenPlaceholder). */
export const tokenPlaceholder = "<one-time-token>";

/** matchFlag extracts the value of a `--flag VALUE` or `--flag=VALUE` occurrence
 * from a command line, or undefined when the flag is absent. */
function matchFlag(command: string, flag: string): string | undefined {
  return command.match(new RegExp(`--${flag}(?:\\s+|=)(\\S+)`))?.[1];
}

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
 * daemon unit uses KillMode=process to keep containers alive across a restart.
 *
 * For a networked firecracker spoke the script also installs a privileged
 * `llmbox-firecracker-network.service` oneshot that provisions the host TAP/NAT
 * egress pool at boot (the CAP_NET_ADMIN work), and runs the spoke with
 * --egress-mode=external so the long-running spoke attaches to that pool without
 * mutating host networking itself. The two units are ordered so the pool exists
 * before the spoke starts. An explicit --egress-mode=managed / --disable-egress in
 * the command opts out (the spoke keeps provisioning / stays control-only). */
export function systemdSetupScript(command: string): string {
  const firecracker = /^llmbox-spoke\s+firecracker\b/.test(command);
  const egressMode = matchFlag(command, "egress-mode");
  // --disable-egress is a boolean flag: bare or =true enables it, =false does not.
  const disableEgressMatch = command.match(/(?:^|\s)--disable-egress(?:=(\S+))?/);
  const disableEgress =
    disableEgressMatch !== null &&
    disableEgressMatch[1] !== "false" &&
    disableEgressMatch[1] !== "0";
  const controlOnly = disableEgress || egressMode === "disabled";
  // Default a networked firecracker spoke to externally managed egress: a root
  // oneshot provisions the pool and the spoke attaches unprivileged. An explicit
  // --egress-mode=managed keeps the spoke provisioning the pool itself.
  const externalEgress = firecracker && !controlOnly && egressMode !== "managed";

  let exec = command
    .replace(/^llmbox-spoke\s+/, "/usr/local/bin/llmbox-spoke ")
    .replace(/\s+--state\s+\S+/, "")
    .replace(/\s+--state-dir\s+\S+/, "");
  if (externalEgress) {
    // Normalise any existing egress-mode to the external one the two-unit setup uses.
    exec = exec.replace(/\s+--egress-mode(?:\s+|=)\S+/, "");
  }
  exec +=
    " --state /var/lib/llmbox/llmbox-spoke.json" +
    (firecracker ? " --state-dir /var/lib/llmbox/firecracker" : "") +
    (externalEgress ? " --egress-mode external" : "");

  const spokeUnit = `sudo tee /etc/systemd/system/${spokeServiceName} >/dev/null <<'UNIT'
[Unit]
Description=llmbox spoke runner
Wants=network-online.target
After=network-online.target${externalEgress ? `\nRequires=${networkServiceName}\nAfter=${networkServiceName}` : ""}

[Service]
ExecStart=${exec}
Restart=on-failure
RestartSec=5
StateDirectory=llmbox${firecracker ? "\nKillMode=process" : ""}

[Install]
WantedBy=multi-user.target
UNIT`;

  if (!externalEgress) {
    return `${spokeUnit}

sudo systemctl daemon-reload
sudo systemctl enable --now ${spokeServiceName}`;
  }

  // The setup command mirrors the spoke's pool knobs so the provisioned pool and the
  // slots the spoke attaches to line up (same size, same owning group).
  const setupArgs = ["--pool-size", "--tap-group"]
    .map((flag) => {
      const v = matchFlag(command, flag.replace(/^--/, ""));
      return v ? `${flag} ${v}` : "";
    })
    .filter(Boolean)
    .join(" ");
  const setupExec =
    "/usr/local/bin/llmbox-spoke firecracker network setup" +
    (setupArgs ? ` ${setupArgs}` : "");

  const networkUnit = `sudo tee /etc/systemd/system/${networkServiceName} >/dev/null <<'UNIT'
[Unit]
Description=llmbox firecracker egress network (TAP/NAT pool)
Wants=network-online.target
After=network-online.target
Before=${spokeServiceName}

[Service]
Type=oneshot
ExecStart=${setupExec}
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
UNIT`;

  return `${networkUnit}

${spokeUnit}

sudo systemctl daemon-reload
sudo systemctl enable --now ${networkServiceName}
sudo systemctl enable --now ${spokeServiceName}`;
}
