import { describe, expect, it } from "vitest";
import { spokeServiceName, systemdSetupScript, tokenPlaceholder } from "./spokeSetup";

const command =
  "llmbox-spoke docker --hub wss://hub.example.com/spoke/connect --token SECRET --state llmbox-spoke.json";

describe("systemdSetupScript", () => {
  it("rewrites the command for service use inside the unit", () => {
    const script = systemdSetupScript(command);
    expect(script).toContain(
      "ExecStart=/usr/local/bin/llmbox-spoke docker --hub wss://hub.example.com/spoke/connect --token SECRET --state /var/lib/llmbox/llmbox-spoke.json",
    );
    // The relative state path must not survive anywhere in the script.
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
});
