// NetworkView is the network-isolation screen: the allowlist groups that decide
// which egress domains a workspace may reach, and how those groups are assigned
// (globally to every workspace, or per workspace). Egress is deny-by-default on
// an isolation-enabled runner, so a workspace reaches only its effective
// allowlist = global groups ∪ its own groups. It owns its own allowlist data
// (independent of the dashboard's boxes/spokes) and reloads it after every edit.
import { useCallback, useEffect, useState } from "react";
import {
  ActionIcon,
  Badge,
  Box,
  Button,
  Card,
  Group,
  Menu,
  SimpleGrid,
  Skeleton,
  Stack,
  Switch,
  Table,
  Tabs,
  Text,
  Title,
  Tooltip,
} from "@mantine/core";
import {
  IconClock,
  IconDotsVertical,
  IconDownload,
  IconEdit,
  IconPlus,
  IconServer2,
  IconShieldLock,
  IconTrash,
  IconUpload,
  IconWorld,
} from "@tabler/icons-react";
import { notifications } from "@mantine/notifications";
import type { Api, AllowlistGroup, BoxView } from "../api";
import { ApiError } from "../api";
import type { DashboardData } from "../lib/data";
import { errorMessage, perform } from "../lib/actions";
import { redirectToSignIn } from "../lib/navigation";
import { confirmDestroy } from "../lib/confirm";
import { boxId } from "../lib/format";
import { GroupEditorModal } from "./GroupEditorModal";
import { ImportGroupsModal } from "./ImportGroupsModal";
import { BoxGroupsModal } from "./BoxGroupsModal";

export interface NetworkViewProps {
  api: Api;
  data: DashboardData | null;
}

/** NetworkView renders the allowlist groups and assignment screen.
 *
 * @arg props The api client and the loaded dashboard data (for the workspace list).
 * @return JSX.Element The network screen.
 */
export function NetworkView({ api, data }: NetworkViewProps): JSX.Element {
  const [groups, setGroups] = useState<AllowlistGroup[] | null>(null);
  const [editing, setEditing] = useState<AllowlistGroup | "new" | null>(null);
  const [importing, setImporting] = useState(false);
  const [assignBox, setAssignBox] = useState<BoxView | null>(null);

  const reload = useCallback(async () => {
    try {
      setGroups(await api.listAllowlistGroups());
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        redirectToSignIn();
        return;
      }
      notifications.show({ color: "red", title: "Couldn't load groups", message: errorMessage(err) });
    }
  }, [api]);

  useEffect(() => {
    void reload();
  }, [reload]);

  const exportAll = () =>
    void perform(
      async () => {
        const bundle = await api.exportAllowlistGroups();
        downloadJSON("llmbox-allowlist.json", bundle);
      },
      { success: "exported allowlist groups" },
    );

  return (
    <Stack gap="lg">
      <Group justify="space-between" align="flex-end" wrap="wrap">
        <Box>
          <Title order={2}>Network</Title>
          <Text c="dimmed" size="sm" maw={640}>
            Workspace egress is deny-by-default. Only DNS is reachable until a domain is allowlisted;
            resolved IPs are opened for a short, configurable window.
          </Text>
        </Box>
        <Group gap="sm">
          <Button
            variant="default"
            leftSection={<IconUpload size={16} />}
            onClick={() => setImporting(true)}
          >
            Import
          </Button>
          <Button variant="default" leftSection={<IconDownload size={16} />} onClick={exportAll}>
            Export
          </Button>
          <Button leftSection={<IconPlus size={16} />} onClick={() => setEditing("new")}>
            New group
          </Button>
        </Group>
      </Group>

      <Tabs defaultValue="groups" keepMounted={false}>
        <Tabs.List>
          <Tabs.Tab value="groups" leftSection={<IconShieldLock size={16} />}>
            Allowlist groups
          </Tabs.Tab>
          <Tabs.Tab value="assignments" leftSection={<IconServer2 size={16} />}>
            Assignments
          </Tabs.Tab>
        </Tabs.List>

        <Tabs.Panel value="groups" pt="lg">
          <GroupsPanel
            groups={groups}
            onNew={() => setEditing("new")}
            onEdit={(g) => setEditing(g)}
            onDelete={(g) =>
              confirmDestroy({
                title: `Delete ${g.name}?`,
                message: `Removing this group unassigns it from every workspace that uses it. This can't be undone.`,
                confirmLabel: "Delete group",
                action: () => api.deleteAllowlistGroup(g.id),
                success: `deleted ${g.name}`,
                refresh: reload,
              })
            }
          />
        </Tabs.Panel>

        <Tabs.Panel value="assignments" pt="lg">
          <AssignmentsPanel
            api={api}
            groups={groups}
            boxes={data?.boxes ?? null}
            onToggleGlobal={(g, on) =>
              void perform(
                () => api.saveAllowlistGroup({ ...g, is_global: on }),
                { success: `${g.name} ${on ? "applied to all" : "no longer global"}`, onDone: reload },
              )
            }
            onEditBox={(b) => setAssignBox(b)}
          />
        </Tabs.Panel>
      </Tabs>

      <GroupEditorModal
        api={api}
        group={editing === "new" ? null : editing}
        opened={editing !== null}
        onClose={() => setEditing(null)}
        onSaved={reload}
      />
      <ImportGroupsModal
        api={api}
        opened={importing}
        onClose={() => setImporting(false)}
        onDone={reload}
      />
      <BoxGroupsModal
        api={api}
        box={assignBox}
        groups={groups ?? []}
        opened={assignBox !== null}
        onClose={() => setAssignBox(null)}
        onSaved={reload}
      />
    </Stack>
  );
}

/** GroupsPanel renders the grid of allowlist group cards. */
function GroupsPanel({
  groups,
  onNew,
  onEdit,
  onDelete,
}: {
  groups: AllowlistGroup[] | null;
  onNew: () => void;
  onEdit: (g: AllowlistGroup) => void;
  onDelete: (g: AllowlistGroup) => void;
}): JSX.Element {
  if (groups === null) {
    return (
      <SimpleGrid cols={{ base: 1, sm: 2, lg: 3 }}>
        {[0, 1, 2].map((i) => (
          <Skeleton key={i} height={160} radius="md" />
        ))}
      </SimpleGrid>
    );
  }
  if (groups.length === 0) {
    return (
      <Card withBorder padding="xl" radius="md">
        <Stack align="center" gap="xs">
          <IconShieldLock size={28} />
          <Text fw={600}>No allowlist groups yet</Text>
          <Text c="dimmed" size="sm" ta="center" maw={420}>
            Create a group of domains a workspace may reach — API providers, package registries — then
            apply it globally or per workspace.
          </Text>
          <Button mt="sm" leftSection={<IconPlus size={16} />} onClick={onNew}>
            New group
          </Button>
        </Stack>
      </Card>
    );
  }
  return (
    <SimpleGrid cols={{ base: 1, sm: 2, lg: 3 }}>
      {groups.map((g) => (
        <GroupCard key={g.id} group={g} onEdit={() => onEdit(g)} onDelete={() => onDelete(g)} />
      ))}
    </SimpleGrid>
  );
}

/** GroupCard is one allowlist group tile: name, description, sample domains, and
 * usage meta, with an edit/delete menu. */
function GroupCard({
  group,
  onEdit,
  onDelete,
}: {
  group: AllowlistGroup;
  onEdit: () => void;
  onDelete: () => void;
}): JSX.Element {
  const shown = group.domains.slice(0, 3);
  const extra = group.domains.length - shown.length;
  return (
    <Card withBorder radius="md" padding="md">
      <Group justify="space-between" wrap="nowrap" mb="xs">
        <Group gap="xs" wrap="nowrap">
          <IconShieldLock size={18} color="var(--mantine-color-brand-5)" />
          <Text fw={650}>{group.name}</Text>
          {group.is_global && (
            <Badge size="sm" variant="light">
              global
            </Badge>
          )}
        </Group>
        <Menu withinPortal position="bottom-end">
          <Menu.Target>
            <ActionIcon variant="subtle" color="gray" aria-label={`Actions for ${group.name}`}>
              <IconDotsVertical size={18} />
            </ActionIcon>
          </Menu.Target>
          <Menu.Dropdown>
            <Menu.Item leftSection={<IconEdit size={16} />} onClick={onEdit}>
              Edit
            </Menu.Item>
            <Menu.Item color="red" leftSection={<IconTrash size={16} />} onClick={onDelete}>
              Delete
            </Menu.Item>
          </Menu.Dropdown>
        </Menu>
      </Group>
      <Text c="dimmed" size="sm" lineClamp={2} mih={40}>
        {group.description || "No description."}
      </Text>
      <Group gap={6} my="sm">
        {shown.map((d) => (
          <Badge key={d} variant="default" ff="monospace" style={{ textTransform: "none" }}>
            {d}
          </Badge>
        ))}
        {extra > 0 && (
          <Badge variant="transparent" c="dimmed">
            +{extra} more
          </Badge>
        )}
      </Group>
      <Group gap="lg" c="dimmed" fz="xs" pt="xs" style={{ borderTop: "1px solid var(--mantine-color-default-border)" }}>
        <Group gap={5}>
          <IconWorld size={14} />
          {group.domains.length} domains
        </Group>
        <Group gap={5}>
          <IconServer2 size={14} />
          {group.is_global ? "All workspaces" : `${group.box_count} workspace${group.box_count === 1 ? "" : "s"}`}
        </Group>
        <Group gap={5}>
          <IconClock size={14} />
          {group.ttl_seconds}s TTL
        </Group>
      </Group>
    </Card>
  );
}

/** AssignmentsPanel renders the global-group toggles and the per-workspace table. */
function AssignmentsPanel({
  api,
  groups,
  boxes,
  onToggleGlobal,
  onEditBox,
}: {
  api: Api;
  groups: AllowlistGroup[] | null;
  boxes: BoxView[] | null;
  onToggleGlobal: (g: AllowlistGroup, on: boolean) => void;
  onEditBox: (b: BoxView) => void;
}): JSX.Element {
  if (groups === null) {
    return <Skeleton height={200} radius="md" />;
  }
  return (
    <Stack gap="xl">
      <Box>
        <Group gap="xs" mb="sm">
          <IconWorld size={16} />
          <Text fw={640}>Applied to all workspaces</Text>
        </Group>
        <Card withBorder radius="md" padding={0}>
          {groups.length === 0 ? (
            <Text c="dimmed" size="sm" p="md">
              No groups yet — create one first.
            </Text>
          ) : (
            groups.map((g, i) => (
              <Group
                key={g.id}
                justify="space-between"
                p="md"
                style={i > 0 ? { borderTop: "1px solid var(--mantine-color-default-border)" } : undefined}
              >
                <Box>
                  <Text fw={600}>{g.name}</Text>
                  <Text c="dimmed" size="xs">
                    {g.description || `${g.domains.length} domains`}
                  </Text>
                </Box>
                <Group gap="sm">
                  {g.is_global && (
                    <Badge size="sm" variant="light">
                      global
                    </Badge>
                  )}
                  <Switch
                    checked={g.is_global}
                    onChange={(e) => onToggleGlobal(g, e.currentTarget.checked)}
                    aria-label={`Apply ${g.name} to all workspaces`}
                  />
                </Group>
              </Group>
            ))
          )}
        </Card>
      </Box>

      <Box>
        <Group gap="xs" mb="sm">
          <IconServer2 size={16} />
          <Text fw={640}>Per-workspace groups</Text>
          <Text c="dimmed" size="xs">
            — set at creation, editable any time
          </Text>
        </Group>
        <PerWorkspaceTable api={api} boxes={boxes} onEditBox={onEditBox} />
      </Box>
    </Stack>
  );
}

/** PerWorkspaceTable lists each workspace with its extra (non-global) groups and
 * effective domain count, fetched per workspace. */
function PerWorkspaceTable({
  api,
  boxes,
  onEditBox,
}: {
  api: Api;
  boxes: BoxView[] | null;
  onEditBox: (b: BoxView) => void;
}): JSX.Element {
  const [rows, setRows] = useState<Record<string, { extra: string[]; count: number }>>({});

  useEffect(() => {
    let cancelled = false;
    if (!boxes) return;
    void (async () => {
      const globals = new Set<string>();
      const acc: Record<string, { extra: string[]; count: number }> = {};
      await Promise.all(
        boxes.map(async (b) => {
          try {
            const al = await api.getBoxAllowlist(boxId(b));
            // effective_groups includes global names; the box's own extras are the
            // non-global ones, which we approximate as effective minus global-only.
            acc[boxId(b)] = {
              extra: al.effective_groups.filter((n) => !globals.has(n)),
              count: al.effective_domains.length,
            };
          } catch {
            acc[boxId(b)] = { extra: [], count: 0 };
          }
        }),
      );
      if (!cancelled) setRows(acc);
    })();
    return () => {
      cancelled = true;
    };
  }, [api, boxes]);

  if (!boxes) {
    return <Skeleton height={120} radius="md" />;
  }
  if (boxes.length === 0) {
    return (
      <Card withBorder radius="md" padding="md">
        <Text c="dimmed" size="sm">
          No workspaces yet.
        </Text>
      </Card>
    );
  }
  return (
    <Card withBorder radius="md" padding={0}>
      <Table.ScrollContainer minWidth={480}>
        <Table verticalSpacing="sm" horizontalSpacing="md">
          <Table.Thead>
            <Table.Tr>
              <Table.Th>Workspace</Table.Th>
              <Table.Th>Runner</Table.Th>
              <Table.Th>Groups (incl. global)</Table.Th>
              <Table.Th ta="right">Domains</Table.Th>
              <Table.Th />
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {boxes.map((b) => {
              const row = rows[boxId(b)];
              return (
                <Table.Tr key={boxId(b)}>
                  <Table.Td ff="monospace">{boxId(b)}</Table.Td>
                  <Table.Td c="dimmed">{b.spoke ?? "—"}</Table.Td>
                  <Table.Td>
                    {row === undefined ? (
                      <Skeleton height={16} width={120} />
                    ) : row.extra.length === 0 ? (
                      <Text c="dimmed" size="sm">
                        global only
                      </Text>
                    ) : (
                      <Group gap={6}>
                        {row.extra.map((n) => (
                          <Badge key={n} variant="default" style={{ textTransform: "none" }}>
                            {n}
                          </Badge>
                        ))}
                      </Group>
                    )}
                  </Table.Td>
                  <Table.Td ta="right">{row?.count ?? "—"}</Table.Td>
                  <Table.Td ta="right">
                    <Tooltip label="Edit groups">
                      <ActionIcon variant="subtle" color="gray" aria-label={`Edit groups for ${boxId(b)}`} onClick={() => onEditBox(b)}>
                        <IconEdit size={16} />
                      </ActionIcon>
                    </Tooltip>
                  </Table.Td>
                </Table.Tr>
              );
            })}
          </Table.Tbody>
        </Table>
      </Table.ScrollContainer>
    </Card>
  );
}

/** downloadJSON triggers a browser download of value as a pretty-printed JSON
 * file — how the Export button hands the operator a portable bundle. */
function downloadJSON(filename: string, value: unknown): void {
  const blob = new Blob([JSON.stringify(value, null, 2)], { type: "application/json" });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  a.click();
  URL.revokeObjectURL(url);
}
