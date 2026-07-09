// Entry point for the activation page (/auth/{token}): the same React +
// Mantine stack as the dashboard, mounted on the auth.html shell. All live
// state comes from the token's JSON state endpoint — the shell is static.
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { MantineProvider } from "@mantine/core";
import "@mantine/core/styles.css";
import "../styles.css";
import { theme } from "../theme";
import { ActivationPage } from "./ActivationPage";

createRoot(document.getElementById("app")!).render(
  <StrictMode>
    <MantineProvider theme={theme} defaultColorScheme="auto">
      <ActivationPage />
    </MantineProvider>
  </StrictMode>,
);
