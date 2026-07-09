// ThemeToggle flips the Mantine color scheme between light and dark. It resolves
// "auto" to the current system scheme before toggling, so the first click always
// visibly changes the theme rather than appearing to do nothing.
import { ActionIcon, useComputedColorScheme, useMantineColorScheme } from "@mantine/core";
import { IconMoon, IconSun } from "@tabler/icons-react";

/** ThemeToggle renders the light/dark switch button.
 *
 * @return JSX.Element The toggle control.
 */
export function ThemeToggle(): JSX.Element {
  const { setColorScheme } = useMantineColorScheme();
  const computed = useComputedColorScheme("light", { getInitialValueInEffect: true });
  const next = computed === "dark" ? "light" : "dark";
  return (
    <ActionIcon
      variant="default"
      size="lg"
      aria-label={`Switch to ${next} theme`}
      onClick={() => setColorScheme(next)}
    >
      {computed === "dark" ? <IconSun size={18} /> : <IconMoon size={18} />}
    </ActionIcon>
  );
}
