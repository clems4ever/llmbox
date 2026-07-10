// SpokeSetupTabs is the runner setup instructions shared by the create-runner
// result and the join-token info modal: a "Run command" tab with the one-line
// enrollment command, and a "systemd service" tab with a script that installs
// the spoke as a service (started now and on every boot). When the command
// carries the token placeholder (re-shown after creation), a notice explains
// that the real token appeared only once and cannot be recovered.
import { Alert, Button, Code, CopyButton, Stack, Tabs, Text } from "@mantine/core";
import { IconCheck, IconCopy, IconInfoCircle, IconSettingsAutomation, IconTerminal2 } from "@tabler/icons-react";
import { spokeServiceName, systemdSetupScript, tokenPlaceholder } from "../lib/spokeSetup";

export interface SpokeSetupTabsProps {
  /** The enrollment command as returned by the server (real token or placeholder). */
  command: string;
}

/** CopyBlock renders a command block with its copy button. */
function CopyBlock({ value, label }: { value: string; label: string }): JSX.Element {
  return (
    <>
      <Code block style={{ whiteSpace: "pre-wrap", overflowWrap: "anywhere" }}>
        {value}
      </Code>
      <CopyButton value={value}>
        {({ copied, copy }) => (
          <Button
            variant="default"
            size="xs"
            style={{ alignSelf: "flex-end" }}
            leftSection={copied ? <IconCheck size={16} /> : <IconCopy size={16} />}
            onClick={copy}
          >
            {copied ? "Copied" : label}
          </Button>
        )}
      </CopyButton>
    </>
  );
}

/** SpokeSetupTabs renders the run-command and systemd setup instructions.
 *
 * @arg props The enrollment command to render the instructions from.
 * @return JSX.Element The tabbed setup instructions.
 */
export function SpokeSetupTabs({ command }: SpokeSetupTabsProps): JSX.Element {
  const placeholder = command.includes(tokenPlaceholder);
  return (
    <Stack gap="sm">
      {placeholder && (
        <Alert variant="light" color="yellow" icon={<IconInfoCircle size={18} />}>
          The one-time token was shown only when the runner was created and cannot be
          recovered — replace <Code>{tokenPlaceholder}</Code> with it before running. If
          it's lost, revoke this token and create the runner again.
        </Alert>
      )}
      <Tabs defaultValue="command" keepMounted={false}>
        <Tabs.List>
          <Tabs.Tab value="command" leftSection={<IconTerminal2 size={16} />}>
            Run command
          </Tabs.Tab>
          <Tabs.Tab value="systemd" leftSection={<IconSettingsAutomation size={16} />}>
            systemd service
          </Tabs.Tab>
        </Tabs.List>

        <Tabs.Panel value="command" pt="sm">
          <Stack gap="sm">
            <CopyBlock value={command} label="Copy command" />
            <Text c="dimmed" size="xs">
              Runs the spoke in the foreground. After first enrollment it reconnects from
              the credential saved next to it; the token is one-time.
            </Text>
          </Stack>
        </Tabs.Panel>

        <Tabs.Panel value="systemd" pt="sm">
          <Stack gap="sm">
            <Text size="sm">
              Installs the spoke as a systemd service; <Code>enable --now</Code> starts it
              immediately and on every boot. The <Code>llmbox-spoke</Code> binary must be
              installed at <Code>/usr/local/bin/llmbox-spoke</Code>.
            </Text>
            <CopyBlock value={systemdSetupScript(command)} label="Copy script" />
            <Text c="dimmed" size="xs">
              Check it with <Code>systemctl status {spokeServiceName}</Code> or follow the
              logs with <Code>journalctl -u {spokeServiceName} -f</Code>. The credential is
              saved under <Code>/var/lib/llmbox</Code>, so restarts don't need the token.
            </Text>
          </Stack>
        </Tabs.Panel>
      </Tabs>
    </Stack>
  );
}
