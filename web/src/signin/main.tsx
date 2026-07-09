// Entry point for the stand-alone sign-in page (/signin): the same React +
// Mantine stack as the dashboard, mounted on the signin.html shell.
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { MantineProvider } from "@mantine/core";
import "@mantine/core/styles.css";
import "../styles.css";
import { theme } from "../theme";
import { SignInPage } from "./SignInPage";

createRoot(document.getElementById("app")!).render(
  <StrictMode>
    <MantineProvider theme={theme} defaultColorScheme="auto">
      <SignInPage />
    </MantineProvider>
  </StrictMode>,
);
