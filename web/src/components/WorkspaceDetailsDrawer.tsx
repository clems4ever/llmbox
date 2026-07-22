// WorkspaceDetailsDrawer is where a single workspace's detail lives — including
// its HTTP proxies, which are intentionally not surfaced anywhere else. It shows
// the box's metadata and, when the hub has the proxy feature enabled, the list
// of proxies fronting this box with controls to add and remove them. It renders
// as a right-hand drawer driven by the selected box (null = closed).
import { useEffect, useRef, useState } from "react";
import {
  ActionIcon,
  Anchor,
  Badge,
  Box,
  Button,
  Code,
  CopyButton,
  Divider,
  Drawer,
  Group,
  Loader,
  Paper,
  SimpleGrid,
  Stack,
  Table,
  Text,
  TextInput,
  Title,
  Tooltip,
} from "@mantine/core";
import { IconCheck, IconCopy, IconPlus, IconTrash } from "@tabler/icons-react";
import type { Api, BoxView, NetworkFlow, ProxyInfo } from "../api";
import { boxId, clockTime, createdAt, formatBytes } from "../lib/format";
import { perform } from "../lib/actions";
import { confirmDestroy } from "../lib/confirm";
import { StatusBadge } from "./StatusBadge";

export interface WorkspaceDetailsDrawerProps {
  api: Api;
  box: BoxView | null;
  proxyEnabled: boolean;
  proxies: ProxyInfo[];
  refresh: () => Promise<void>;
  onClose: () => void;
}

/** WorkspaceDetailsDrawer renders the selected workspace's metadata and proxies.
 *
 * @arg props The api client, the selected box (null = closed), its proxies, and callbacks.
 * @return JSX.Element The drawer.
 */
export function WorkspaceDetailsDrawer({
  api,
  box,
  proxyEnabled,
  proxies,
  refresh,
  onClose,
}: WorkspaceDetailsDrawerProps): JSX.Element {
  const id = box ? boxId(box) : "";
  return (
    <Drawer
      opened={box !== null}
      onClose={onClose}
      position="right"
      size="lg"
      padding="lg"
      title={
        <Group gap="sm">
          <Title order={3} className="mono-wrap">{id}</Title>
          {box && <StatusBadge phase={box.phase} />}
        </Group>
      }
    >
      {box && (
        <Stack gap="lg">
          <Metadata box={box} />
          <Divider />
          {proxyEnabled ? (
            <ProxiesSection api={api} boxId={id} proxies={proxies} refresh={refresh} />
          ) : (
            <Text c="dimmed" size="sm">
              The reverse proxy is not enabled on this hub, so this workspace has no
              HTTP proxies.
            </Text>
          )}
          <Divider />
          <NetworkSection api={api} boxId={id} />
        </Stack>
      )}
    </Drawer>
  );
}

/** Metadata renders the workspace's descriptive fields as a compact grid. */
function Metadata({ box }: { box: BoxView }): JSX.Element {
  return (
    <Stack gap="sm">
      {box.description && <Text size="sm">{box.description}</Text>}
      <SimpleGrid cols={2} spacing="sm" verticalSpacing="xs">
        <Field label="Runner" value={box.spoke || "—"} mono />
        <Field label="Image" value={box.image} mono />
        <Field label="State" value={box.state} />
        <Field label="Created" value={createdAt(box.created) || "—"} />
      </SimpleGrid>
      {box.phase === "broken" && <InitScriptFailure output={box.last_error} />}
    </Stack>
  );
}

/** InitScriptFailure surfaces why a broken workspace's init script failed: the
 * captured output the spoke reported, so an operator can diagnose it without
 * shelling into the box. */
function InitScriptFailure({ output }: { output?: string }): JSX.Element {
  return (
    <Paper withBorder radius="md" p="sm" data-broken-init-script>
      <Text size="xs" c="red" tt="uppercase" fw={700} mb={4}>
        Init script failed
      </Text>
      <Text size="xs" c="dimmed" mb="xs">
        The workspace's provisioning script failed, so it never started. Its output:
      </Text>
      <Code block>{output?.trim() || "(no output captured)"}</Code>
    </Paper>
  );
}

/** Field renders one label/value pair in the metadata grid. */
function Field({ label, value, mono }: { label: string; value: string; mono?: boolean }): JSX.Element {
  return (
    <Box>
      <Text size="xs" c="dimmed" tt="uppercase" fw={600}>{label}</Text>
      <Text size="sm" className={mono ? "mono-wrap" : undefined}>{value}</Text>
    </Box>
  );
}

/** networkRefreshMs is how often the network-audit table re-polls the box's flows
 * while the drawer is open, giving the view a live feel without hammering the hub. */
const networkRefreshMs = 3000;

/** NetworkSection shows a live, per-box audit of the box's outbound network flows
 * — which destinations it connected out to and how much data moved — built from
 * the host's connection-tracking metadata (never packet payloads). It polls the
 * box-network endpoint while the drawer is open and stops when it closes. An
 * unaudited box (no egress, or a spoke that cannot read conntrack) simply shows an
 * empty table. */
function NetworkSection({ api, boxId }: { api: Api; boxId: string }): JSX.Element {
  const [flows, setFlows] = useState<NetworkFlow[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  // Keep the latest box id in a ref so a slow in-flight fetch that resolves after
  // the drawer switched boxes does not write another box's flows into this view.
  const currentBox = useRef(boxId);
  currentBox.current = boxId;

  useEffect(() => {
    let active = true;
    const load = async () => {
      try {
        const f = await api.boxNetwork(boxId);
        if (active && currentBox.current === boxId) {
          setFlows(f);
          setError(null);
        }
      } catch (e) {
        if (active && currentBox.current === boxId) {
          setError(e instanceof Error ? e.message : String(e));
        }
      }
    };
    setFlows(null);
    setError(null);
    void load();
    const timer = setInterval(load, networkRefreshMs);
    return () => {
      active = false;
      clearInterval(timer);
    };
  }, [api, boxId]);

  return (
    <Stack gap="sm" id="network-section">
      <Box>
        <Group gap="xs" align="center">
          <Title order={4}>Network activity</Title>
          {flows !== null && (
            <Badge variant="light" color="gray" size="sm" data-network-count>
              {flows.length} {flows.length === 1 ? "flow" : "flows"}
            </Badge>
          )}
        </Group>
        <Text c="dimmed" size="sm">
          Audited outbound connections this workspace made — destinations and byte
          counts from the host's connection tracker, not packet contents. Updates live.
        </Text>
      </Box>

      {error ? (
        <Text c="red" size="sm" data-network-error>
          Could not load network activity: {error}
        </Text>
      ) : flows === null ? (
        <Group gap="xs" c="dimmed">
          <Loader size="xs" />
          <Text size="sm">Loading network activity…</Text>
        </Group>
      ) : flows.length === 0 ? (
        <Text c="dimmed" size="sm">
          No outbound connections recorded for this workspace yet.
        </Text>
      ) : (
        <Paper withBorder radius="md">
          <Table verticalSpacing="sm" horizontalSpacing="md" data-network-table>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>When</Table.Th>
                <Table.Th>Destination</Table.Th>
                <Table.Th>Proto</Table.Th>
                <Table.Th ta="right">Out</Table.Th>
                <Table.Th ta="right">In</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {flows.map((f) => (
                <Table.Tr key={flowKey(f)} data-network-row={flowKey(f)}>
                  <Table.Td className="mono-wrap">{clockTime(f.last_seen)}</Table.Td>
                  <Table.Td>
                    <Text className="mono-wrap" size="sm">
                      {f.dst_ip}
                      {f.dst_port ? `:${f.dst_port}` : ""}
                    </Text>
                    {f.state && (
                      <Text c="dimmed" size="xs">
                        {f.state}
                      </Text>
                    )}
                  </Table.Td>
                  <Table.Td>
                    <Badge variant="light" color="blue" size="sm">
                      {f.proto}
                    </Badge>
                  </Table.Td>
                  <Table.Td ta="right" className="mono-wrap">{formatBytes(f.bytes_out)}</Table.Td>
                  <Table.Td ta="right" className="mono-wrap">{formatBytes(f.bytes_in)}</Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Paper>
      )}
    </Stack>
  );
}

/** flowKey is a stable React key (and test handle) for a flow row: its 4-tuple,
 * which uniquely identifies the connection within a box. */
function flowKey(f: NetworkFlow): string {
  return `${f.proto}:${f.src_port ?? 0}:${f.dst_ip}:${f.dst_port ?? 0}`;
}

interface ProxiesSectionProps {
  api: Api;
  boxId: string;
  proxies: ProxyInfo[];
  refresh: () => Promise<void>;
}

/** ProxiesSection lists this box's proxies and hosts the add-proxy form. */
function ProxiesSection({ api, boxId, proxies, refresh }: ProxiesSectionProps): JSX.Element {
  const removeProxy = (p: ProxyInfo) => {
    confirmDestroy({
      title: "Remove proxy",
      message: `Remove the proxy for ${p.box_id}:${p.port}?`,
      action: () => api.deleteProxy(p.box_id, p.port),
      success: `removed proxy ${p.box_id}:${p.port}`,
      refresh,
    });
  };

  return (
    <Stack gap="sm" id="proxies-section">
      <Box>
        <Title order={4}>HTTP proxies</Title>
        <Text c="dimmed" size="sm">
          Public URLs that forward to a port inside this workspace.
        </Text>
      </Box>

      {proxies.length === 0 ? (
        <Text c="dimmed" size="sm">No proxies for this workspace yet.</Text>
      ) : (
        <Paper withBorder radius="md">
          <Table verticalSpacing="sm" horizontalSpacing="md">
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Port</Table.Th>
                <Table.Th>URL</Table.Th>
                <Table.Th />
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {proxies.map((p) => (
                <Table.Tr key={p.port} data-proxy-row={`${p.box_id}:${p.port}`}>
                  <Table.Td className="mono-wrap">{p.port}</Table.Td>
                  <Table.Td>
                    <Group gap={6} wrap="nowrap">
                      <Anchor href={p.url} target="_blank" rel="noopener" className="mono-wrap" size="sm">
                        {p.url}
                      </Anchor>
                      <CopyButton value={p.url}>
                        {({ copied, copy }) => (
                          <Tooltip label={copied ? "Copied" : "Copy URL"}>
                            <ActionIcon variant="subtle" size="sm" aria-label="Copy URL" onClick={copy}>
                              {copied ? <IconCheck size={14} /> : <IconCopy size={14} />}
                            </ActionIcon>
                          </Tooltip>
                        )}
                      </CopyButton>
                    </Group>
                    {p.description && <Text c="dimmed" size="xs">{p.description}</Text>}
                  </Table.Td>
                  <Table.Td ta="right">
                    <Tooltip label="Remove proxy">
                      <ActionIcon
                        variant="subtle"
                        color="red"
                        data-proxy={`${p.box_id}:${p.port}`}
                        aria-label={`Remove proxy ${p.port}`}
                        onClick={() => removeProxy(p)}
                      >
                        <IconTrash size={16} />
                      </ActionIcon>
                    </Tooltip>
                  </Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Paper>
      )}

      <AddProxyForm api={api} boxId={boxId} refresh={refresh} />
    </Stack>
  );
}

/** AddProxyForm is the inline add-a-proxy row for the current workspace. */
function AddProxyForm({
  api,
  boxId,
  refresh,
}: {
  api: Api;
  boxId: string;
  refresh: () => Promise<void>;
}): JSX.Element {
  const [port, setPort] = useState("");
  const [description, setDescription] = useState("");
  const [submitting, setSubmitting] = useState(false);

  const submit = async () => {
    const portNum = parseInt(port, 10);
    setSubmitting(true);
    const ok = await perform(
      () => api.createProxy(boxId, Number.isFinite(portNum) ? portNum : 0, description.trim()),
      { success: `created proxy for ${boxId}:${port}`, onDone: refresh },
    );
    setSubmitting(false);
    if (ok) {
      setPort("");
      setDescription("");
    }
  };

  return (
    <Paper withBorder radius="md" p="md">
      <form
        id="create-proxy-form"
        onSubmit={(e) => {
          e.preventDefault();
          void submit();
        }}
      >
        <Group align="flex-end" gap="sm" wrap="wrap">
          <TextInput
            required
            label="Port"
            name="port"
            placeholder="8000"
            w={100}
            value={port}
            onChange={(e) => setPort(e.currentTarget.value)}
          />
          <TextInput
            label="Description"
            name="description"
            placeholder="Optional"
            style={{ flex: 1, minWidth: 140 }}
            value={description}
            onChange={(e) => setDescription(e.currentTarget.value)}
          />
          <Button
            type="submit"
            leftSection={<IconPlus size={16} />}
            loading={submitting}
            disabled={!port.trim()}
          >
            Add proxy
          </Button>
        </Group>
      </form>
    </Paper>
  );
}
