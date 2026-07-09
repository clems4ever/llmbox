// Entry point for the llmbox workspaces dashboard: a React + Mantine single-page
// app over the box-control API (/api/v1) — the same authenticated API llmbox-mcp
// and scripts drive, here with the login cookie + CSRF header instead of a
// bearer key. It mounts the provider stack (theme, notifications) and boots the
// session in <App>.
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { MantineProvider } from "@mantine/core";
import { ModalsProvider } from "@mantine/modals";
import { Notifications } from "@mantine/notifications";
import "@mantine/core/styles.css";
import "@mantine/notifications/styles.css";
import "./styles.css";
import { theme } from "./theme";
import { App } from "./App";

createRoot(document.getElementById("app")!).render(
  <StrictMode>
    <MantineProvider theme={theme} defaultColorScheme="auto">
      <ModalsProvider>
        <Notifications position="top-right" />
        <App />
      </ModalsProvider>
    </MantineProvider>
  </StrictMode>,
);
