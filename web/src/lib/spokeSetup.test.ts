import { describe, expect, it } from "vitest";
import { spokeServiceName, systemdSetupScript, tokenPlaceholder } from "./spokeSetup";

const command =
  "llmbox-spoke docker --hub wss://hub.example.com/spoke/connect --token SECRET";

describe("systemdSetupScript", () => {
  it("rewrites the command for service use inside the unit", () => {
    const script = systemdSetupScript(command);
    expect(script).toContain(
      "ExecStart=/usr/local/bin/llmbox-spoke docker --hub wss://hub.example.com/spoke/connect --token SECRET --state /var/lib/llmbox/llmbox-spoke.json",
    );
  });

  it("replaces an explicit --state with the service location", () => {
    const script = systemdSetupScript(`${command} --state llmbox-spoke.json`);
    expect(script).toContain("--state /var/lib/llmbox/llmbox-spoke.json");
    // The original state path must not survive anywhere in the script.
    expect(script).not.toContain("--state llmbox-spoke.json");
  });

  it("installs, reloads, and enables the service", () => {
    const script = systemdSetupScript(command);
    expect(script).toContain(`sudo tee /etc/systemd/system/${spokeServiceName}`);
    expect(script).toContain("sudo systemctl daemon-reload");
    expect(script).toContain(`sudo systemctl enable --now ${spokeServiceName}`);
    expect(script).toContain("StateDirectory=llmbox");
  });

  it("passes the token placeholder through untouched", () => {
    const script = systemdSetupScript(command.replace("SECRET", tokenPlaceholder));
    expect(script).toContain(`--token ${tokenPlaceholder}`);
  });

  it("does not pin a firecracker state dir for a docker spoke", () => {
    expect(systemdSetupScript(command)).not.toContain("--state-dir");
  });

  it("does not set KillMode for a docker spoke", () => {
    // Docker containers are owned by dockerd, not the spoke, so the default
    // control-group kill is fine — only firecracker needs KillMode=process.
    expect(systemdSetupScript(command)).not.toContain("KillMode");
  });

  it("sets KillMode=process for a firecracker spoke so its VMs survive a restart", () => {
    const fc = "llmbox-spoke firecracker --hub wss://hub.example.com/spoke/connect --token SECRET";
    expect(systemdSetupScript(fc)).toContain("KillMode=process");
  });

  it("pins the firecracker state dir off tmpfs for a firecracker spoke", () => {
    const fc = "llmbox-spoke firecracker --hub wss://hub.example.com/spoke/connect --token SECRET";
    const script = systemdSetupScript(fc);
    expect(script).toContain("--state-dir /var/lib/llmbox/firecracker");
  });

  it("replaces an explicit firecracker --state-dir with the service location", () => {
    const fc =
      "llmbox-spoke firecracker --hub wss://h/spoke/connect --token SECRET --state-dir /run/llmbox/firecracker";
    const script = systemdSetupScript(fc);
    expect(script).toContain("--state-dir /var/lib/llmbox/firecracker");
    expect(script).not.toContain("--state-dir /run/llmbox/firecracker");
  });
});
