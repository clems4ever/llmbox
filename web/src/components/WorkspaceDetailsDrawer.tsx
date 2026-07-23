// WorkspaceDetailsDrawer is where a single workspace's detail lives — including
// its HTTP proxies, which are intentionally not surfaced anywhere else. It shows
// the box's metadata and, when the hub has the proxy feature enabled, the list
// of proxies fronting this box with controls to add and remove them. It renders
// as a right-hand drawer driven by the selected box (null = closed).
import { useCallback, useEffect, useState } from "react";
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
  Paper,
  SimpleGrid,
  Skeleton,
  Stack,
  Table,
  Text,
  TextInput,
  Title,
  Tooltip,
} from "@mantine/core";
import { IconCheck, IconCopy, IconEdit, IconPlus, IconShieldLock, IconTrash } from "@tabler/icons-react";
import type { AllowlistGroup, Api, BoxAllowlist, BoxView, ProxyInfo } from "../api";
import { boxId, createdAt } from "../lib/format";
import { perform } from "../lib/actions";
import { confirmDestroy } from "../lib/confirm";
import { StatusBadge } from "./StatusBadge";
import { BoxGroupsModal } from "./BoxGroupsModal";

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
          <NetworkSection api={api} box={box} />
          <Divider />
          {proxyEnabled ? (
            <ProxiesSection api={api} boxId={id} proxies={proxies} refresh={refresh} />
          ) : (
            <Text c="dimmed" size="sm">
              The reverse proxy is not enabled on this hub, so this workspace has no
              HTTP proxies.
            </Text>
          )}
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

/** NetworkSection shows this workspace's effective allowlist — the groups it may
 * reach (global groups plus its own) and the flattened domain set — with a button
 * to edit its per-workspace group assignment. It loads its own data so the drawer
 * stays independent of the dashboard payload. */
function NetworkSection({ api, box }: { api: Api; box: BoxView }): JSX.Element {
  const id = boxId(box);
  const [allowlist, setAllowlist] = useState<BoxAllowlist | null>(null);
  const [groups, setGroups] = useState<AllowlistGroup[]>([]);
  const [editing, setEditing] = useState(false);

  const reload = useCallback(async () => {
    const [al, gs] = await Promise.all([api.getBoxAllowlist(id), api.listAllowlistGroups()]);
    setAllowlist(al);
    setGroups(gs);
  }, [api, id]);

  useEffect(() => {
    void reload().catch(() => {
      // Network isolation may not be configured; leave the section in its empty state.
    });
  }, [reload]);

  const domains = allowlist?.effective_domains ?? [];
  const shown = domains.slice(0, 8);
  return (
    <Stack gap="sm">
      <Group justify="space-between">
        <Group gap="xs">
          <IconShieldLock size={16} />
          <Text fw={640}>Network</Text>
        </Group>
        <Button
          size="xs"
          variant="default"
          leftSection={<IconEdit size={14} />}
          onClick={() => setEditing(true)}
          disabled={groups.length === 0}
        >
          Edit groups
        </Button>
      </Group>
      {allowlist === null ? (
        <Skeleton height={48} radius="md" />
      ) : (
        <>
          <Text c="dimmed" size="xs">
            Egress deny-by-default. Reaches {domains.length} domain{domains.length === 1 ? "" : "s"} across{" "}
            {allowlist.effective_groups.length} group{allowlist.effective_groups.length === 1 ? "" : "s"}.
          </Text>
          {allowlist.effective_groups.length > 0 && (
            <Group gap={6}>
              {allowlist.effective_groups.map((n) => (
                <Badge key={n} variant="light" style={{ textTransform: "none" }}>
                  {n}
                </Badge>
              ))}
            </Group>
          )}
          {shown.length > 0 && (
            <Group gap={6}>
              {shown.map((d) => (
                <Badge key={d} variant="default" ff="monospace" style={{ textTransform: "none" }}>
                  {d}
                </Badge>
              ))}
              {domains.length > shown.length && (
                <Badge variant="transparent" c="dimmed">
                  +{domains.length - shown.length} more
                </Badge>
              )}
            </Group>
          )}
        </>
      )}
      <BoxGroupsModal
        api={api}
        box={editing ? box : null}
        groups={groups}
        opened={editing}
        onClose={() => setEditing(false)}
        onSaved={reload}
      />
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
