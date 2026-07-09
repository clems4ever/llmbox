// Dashboard is the authenticated shell: an AppShell with a section navbar
// (Workspaces / Infrastructure) and a header, owning the one copy of dashboard
// data and the refresh that reloads it. It routes between the two views and
// hosts the workspace-details drawer. Everything it renders receives the loaded
// data plus `api` and `refresh`, so the child views never fetch on their own.
import { useCallback, useEffect, useState } from "react";
import {
  ActionIcon,
  AppShell,
  Badge,
  Burger,
  Group,
  NavLink,
  ScrollArea,
  Tooltip,
} from "@mantine/core";
import { useDisclosure } from "@mantine/hooks";
import { IconApps, IconRefresh, IconServer2 } from "@tabler/icons-react";
import { Api, ApiError, type Me } from "../api";
import { errorMessage } from "../lib/actions";
import { redirectToSignIn } from "../lib/navigation";
import { loadDashboard, proxiesFor, type DashboardData } from "../lib/data";
import { boxId } from "../lib/format";
import { notifications } from "@mantine/notifications";
import { Brand } from "./Brand";
import { ThemeToggle } from "./ThemeToggle";
import { WorkspacesView } from "./WorkspacesView";
import { InfrastructureView } from "./InfrastructureView";
import { WorkspaceDetailsDrawer } from "./WorkspaceDetailsDrawer";

export type View = "workspaces" | "infrastructure";

export interface DashboardProps {
  api: Api;
  session: Me;
}

/** Dashboard renders the admin shell and the currently selected view.
 *
 * @arg props The authenticated api client and the signed-in session.
 * @return JSX.Element The dashboard shell.
 */
export function Dashboard({ api, session }: DashboardProps): JSX.Element {
  const [data, setData] = useState<DashboardData | null>(null);
  const [loading, setLoading] = useState(true);
  const [view, setView] = useState<View>("workspaces");
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [navOpened, { toggle: toggleNav, close: closeNav }] = useDisclosure(false);

  const refresh = useCallback(async () => {
    setLoading(true);
    try {
      setData(await loadDashboard(api));
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        redirectToSignIn();
        return;
      }
      notifications.show({ color: "red", title: "Couldn't refresh", message: errorMessage(err) });
    } finally {
      setLoading(false);
    }
  }, [api]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const go = (v: View) => {
    setView(v);
    closeNav();
  };

  const selected = data?.boxes.find((b) => boxId(b) === selectedId) ?? null;

  return (
    <AppShell
      header={{ height: 60 }}
      navbar={{ width: 250, breakpoint: "sm", collapsed: { mobile: !navOpened } }}
      padding="md"
    >
      <AppShell.Header>
        <Group h="100%" px="md" justify="space-between" wrap="nowrap">
          <Group gap="sm" wrap="nowrap">
            <Burger opened={navOpened} onClick={toggleNav} hiddenFrom="sm" size="sm" />
            <Brand />
          </Group>
          <Group gap="xs" wrap="nowrap">
            <Tooltip label="Refresh">
              <ActionIcon
                variant="default"
                size="lg"
                aria-label="Refresh"
                loading={loading}
                onClick={() => void refresh()}
              >
                <IconRefresh size={18} />
              </ActionIcon>
            </Tooltip>
            <ThemeToggle />
          </Group>
        </Group>
      </AppShell.Header>

      <AppShell.Navbar p="sm">
        <AppShell.Section grow component={ScrollArea}>
          <NavLink
            active={view === "workspaces"}
            label="Workspaces"
            leftSection={<IconApps size={18} />}
            rightSection={data ? <Badge size="sm" variant="light" circle>{data.boxes.length}</Badge> : null}
            onClick={() => go("workspaces")}
          />
          <NavLink
            active={view === "infrastructure"}
            label="Infrastructure"
            leftSection={<IconServer2 size={18} />}
            rightSection={data ? <Badge size="sm" variant="light" circle>{data.spokes.length}</Badge> : null}
            onClick={() => go("infrastructure")}
          />
        </AppShell.Section>
        <AppShell.Section>
          <Group px="xs" py="xs">
            <Brand email={session.email} />
          </Group>
        </AppShell.Section>
      </AppShell.Navbar>

      <AppShell.Main>
        {view === "workspaces" && (
          <WorkspacesView
            api={api}
            data={data}
            refresh={refresh}
            onSelect={(id) => setSelectedId(id)}
          />
        )}
        {view === "infrastructure" && (
          <InfrastructureView api={api} data={data} refresh={refresh} />
        )}
      </AppShell.Main>

      <WorkspaceDetailsDrawer
        api={api}
        box={selected}
        proxyEnabled={data?.proxyEnabled ?? false}
        proxies={selected ? proxiesFor(data?.proxies ?? [], selectedId!) : []}
        refresh={refresh}
        onClose={() => setSelectedId(null)}
      />
    </AppShell>
  );
}
