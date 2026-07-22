import { describe, expect, it } from "vitest";
import {
  networkServiceName,
  spokeServiceName,
  systemdSetupScript,
  tokenPlaceholder,
} from "./spokeSetup";

const command =
  "llmbox-spoke docker --hub wss://hub.example.com/spoke/connect --token SECRET";
const fcCommand =
  "llmbox-spoke firecracker --hub wss://hub.example.com/spoke/connect --token SECRET";

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

  it("installs a root network oneshot and runs a firecracker spoke unprivileged of host networking", () => {
    const script = systemdSetupScript(fcCommand);
    // The privileged TAP/NAT provisioning lives in its own oneshot unit...
    expect(script).toContain(`sudo tee /etc/systemd/system/${networkServiceName}`);
    expect(script).toContain("Type=oneshot");
    expect(script).toContain("RemainAfterExit=yes");
    expect(script).toContain(
      "ExecStart=/usr/local/bin/llmbox-spoke firecracker network setup",
    );
    // ...and the spoke attaches to it in external mode, ordered after it.
    expect(script).toContain("--egress-mode external");
    expect(script).toContain(`Requires=${networkServiceName}`);
    expect(script).toContain(`After=${networkServiceName}`);
    expect(script).toContain(`Before=${spokeServiceName}`);
    expect(script).toContain(`sudo systemctl enable --now ${networkServiceName}`);
    expect(script).toContain(`sudo systemctl enable --now ${spokeServiceName}`);
  });

  it("does not install a network unit for a docker spoke", () => {
    const script = systemdSetupScript(command);
    expect(script).not.toContain(networkServiceName);
    expect(script).not.toContain("--egress-mode");
  });

  it("does not install a network unit for a control-only firecracker spoke", () => {
    for (const flag of ["--disable-egress", "--egress-mode disabled"]) {
      const script = systemdSetupScript(`${fcCommand} ${flag}`);
      expect(script).not.toContain(networkServiceName);
      expect(script).not.toContain("--egress-mode external");
      // The control-only opt-out is preserved verbatim in the spoke ExecStart.
      expect(script).toContain(flag);
    }
  });

  it("treats --disable-egress=false as egress-on (installs the external network unit)", () => {
    const script = systemdSetupScript(`${fcCommand} --disable-egress=false`);
    expect(script).toContain(networkServiceName);
    expect(script).toContain("--egress-mode external");
  });

  it("keeps the spoke provisioning the pool when --egress-mode=managed is explicit", () => {
    const script = systemdSetupScript(`${fcCommand} --egress-mode managed`);
    expect(script).not.toContain(networkServiceName);
    expect(script).toContain("--egress-mode managed");
    expect(script).not.toContain("--egress-mode external");
  });

  it("mirrors the pool knobs onto the network setup command so pool and spoke agree", () => {
    const script = systemdSetupScript(`${fcCommand} --pool-size 8 --tap-group 4242`);
    expect(script).toContain(
      "ExecStart=/usr/local/bin/llmbox-spoke firecracker network setup --pool-size 8 --tap-group 4242",
    );
    // The spoke keeps the same knobs alongside its external mode.
    expect(script).toContain("--pool-size 8");
    expect(script).toContain("--tap-group 4242");
    expect(script).toContain("--egress-mode external");
  });

  it("normalises an explicit --egress-mode=external to a single external flag", () => {
    const script = systemdSetupScript(`${fcCommand} --egress-mode external`);
    expect(script).toContain(networkServiceName);
    expect(script.match(/--egress-mode external/g)?.length).toBe(1);
  });
});
