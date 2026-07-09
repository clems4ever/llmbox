// Brand is the product wordmark: the llmbox logo beside its name, with an
// optional signed-in identity. Used on the boot screens and, compactly, in the
// app header.
import { Group, Image, Text } from "@mantine/core";

export interface BrandProps {
  /** When set, the signed-in email is shown trailing the wordmark. */
  email?: string;
}

/** Brand renders the logo + "llmbox" wordmark, optionally with the identity.
 *
 * @arg props Optional signed-in email to display.
 * @return JSX.Element The brand line.
 */
export function Brand({ email }: BrandProps): JSX.Element {
  return (
    <Group gap="xs" wrap="nowrap">
      <Image src="/favicon.svg" w={26} h={26} alt="" />
      <Text fw={700} size="lg" style={{ letterSpacing: "-0.01em" }}>
        llmbox
      </Text>
      {email && (
        <Text c="dimmed" size="sm" ml="auto">
          {email}
        </Text>
      )}
    </Group>
  );
}
