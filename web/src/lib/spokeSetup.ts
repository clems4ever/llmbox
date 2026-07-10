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
 * user's home, which is the wrong place for a system service. Keeping --token
 * in the unit is safe: the spoke only uses it on first enrollment and
 * reconnects from the saved credential afterwards. */
export function systemdSetupScript(command: string): string {
  const exec =
    command
      .replace(/^llmbox-spoke\s+/, "/usr/local/bin/llmbox-spoke ")
      .replace(/\s+--state\s+\S+/, "") + " --state /var/lib/llmbox/llmbox-spoke.json";
  return `sudo tee /etc/systemd/system/${spokeServiceName} >/dev/null <<'UNIT'
[Unit]
Description=llmbox spoke runner
Wants=network-online.target
After=network-online.target

[Service]
ExecStart=${exec}
Restart=on-failure
RestartSec=5
StateDirectory=llmbox

[Install]
WantedBy=multi-user.target
UNIT

sudo systemctl daemon-reload
sudo systemctl enable --now ${spokeServiceName}`;
}
