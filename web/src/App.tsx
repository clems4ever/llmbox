// App is the session boot gate. It turns the browser's login cookie into an API
// session via /api/v1/me, then routes to one of four terminal states:
//   - 401  → bounce to the hub's sign-in page
//   - error → a full-page alert
//   - non-admin session → a "not an administrator" notice
//   - admin session → the full <Dashboard>
// Everything below <App> assumes an authenticated admin Api, so this is the one
// place that deals with the un-booted world.
import { useEffect, useState } from "react";
import {
  Alert,
  Center,
  Container,
  Loader,
  Paper,
  Stack,
  Text,
  Title,
} from "@mantine/core";
import { IconAlertTriangle, IconLock } from "@tabler/icons-react";
import { Api, ApiError, me, type Me } from "./api";
import { errorMessage } from "./lib/actions";
import { redirectToSignIn } from "./lib/navigation";
import { Brand } from "./components/Brand";
import { Dashboard } from "./components/Dashboard";

type BootState =
  | { status: "loading" }
  | { status: "redirecting" }
  | { status: "error"; message: string }
  | { status: "not-admin"; session: Me }
  | { status: "ready"; session: Me; api: Api };

/** App boots the API session and renders the dashboard (or a boot state).
 *
 * @return JSX.Element The current boot screen.
 */
export function App(): JSX.Element {
  const [state, setState] = useState<BootState>({ status: "loading" });

  useEffect(() => {
    let cancelled = false;
    me()
      .then((session) => {
        if (cancelled) return;
        if (!session.admin) {
          setState({ status: "not-admin", session });
          return;
        }
        setState({ status: "ready", session, api: new Api(session.csrf) });
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        if (err instanceof ApiError && err.status === 401) {
          setState({ status: "redirecting" });
          redirectToSignIn();
          return;
        }
        setState({ status: "error", message: errorMessage(err) });
      });
    return () => {
      cancelled = true;
    };
  }, []);

  if (state.status === "loading" || state.status === "redirecting") {
    return (
      <Center mih="100vh">
        <Stack align="center" gap="sm">
          <Loader color="brand" />
          <Text c="dimmed" size="sm">
            {state.status === "redirecting" ? "Redirecting to sign in…" : "Loading…"}
          </Text>
        </Stack>
      </Center>
    );
  }

  if (state.status === "error") {
    return (
      <Container size="sm" py="xl">
        <Brand />
        <Alert
          mt="lg"
          color="red"
          variant="light"
          icon={<IconAlertTriangle size={18} />}
          title="Couldn't load the dashboard"
        >
          {state.message}
        </Alert>
      </Container>
    );
  }

  if (state.status === "not-admin") {
    return (
      <Container size="sm" py="xl">
        <Brand email={state.session.email} />
        <Paper mt="lg" p="lg" radius="md" withBorder>
          <Stack gap="xs" align="center" ta="center">
            <IconLock size={28} />
            <Title order={3}>Administrator access required</Title>
            <Text c="dimmed" size="sm">
              {state.session.email} isn't an administrator on this hub. Ask an
              operator to add it to <Text span ff="monospace">auth.admin.emails</Text>.
            </Text>
          </Stack>
        </Paper>
      </Container>
    );
  }

  return <Dashboard api={state.api} session={state.session} />;
}
