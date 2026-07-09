// The Mantine theme that carries llmbox's warm terracotta identity into the
// component library — the same palette the hub's server-rendered sign-in/auth
// pages use (internal/hub/templates/*.tmpl), so the SPA and those pages stay
// visually of a piece. `brand` is the terracotta accent scale (shade 5 is the
// signature #d97757); `dark` is overridden to the warm near-black surfaces the
// old stylesheet used, instead of Mantine's cool default grays.
import { createTheme, type MantineColorsTuple } from "@mantine/core";

const brand: MantineColorsTuple = [
  "#fdf4f0",
  "#f8e2da",
  "#efc4b6",
  "#e6a68f",
  "#df8b6c",
  "#d97757", // signature accent
  "#c8613f",
  "#c2410c", // accent-strong (hover / active)
  "#a5370a",
  "#852d08",
];

const dark: MantineColorsTuple = [
  "#e9e7e2",
  "#c9c6bf",
  "#a6a29a",
  "#7d7970",
  "#4a463f",
  "#38352f",
  "#242220", // card surface
  "#1a1916", // body background
  "#141310",
  "#0e0d0b",
];

export const theme = createTheme({
  primaryColor: "brand",
  // The accent sits at shade 5 in both schemes so filled controls read as the
  // recognizable #d97757 rather than Mantine's default shade-6 pick.
  primaryShade: { light: 5, dark: 5 },
  colors: { brand, dark },
  fontFamily:
    'ui-sans-serif, system-ui, -apple-system, "Segoe UI", Roboto, sans-serif',
  fontFamilyMonospace: "ui-monospace, SFMono-Regular, Menlo, monospace",
  defaultRadius: "md",
  headings: { fontWeight: "650" },
});
