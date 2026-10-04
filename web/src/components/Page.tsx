import type { ReactNode } from "react";
import { Section } from "@astryxdesign/core/Section";
import { HStack, VStack } from "@astryxdesign/core/Stack";
import { Heading, Text } from "@astryxdesign/core/Text";
import { Spinner } from "@astryxdesign/core/Spinner";
import { EmptyState } from "@astryxdesign/core/EmptyState";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Center } from "@astryxdesign/core/Center";

/**
 * Standard page frame: monospace eyebrow, title, supporting copy and a
 * right-aligned action slot that wraps below the title on narrow screens.
 */
export function Page({
  eyebrow,
  title,
  description,
  action,
  children,
}: {
  eyebrow: string;
  title: string;
  description?: string;
  action?: ReactNode;
  children: ReactNode;
}) {
  return (
    <Section variant="transparent" padding={4} paddingBlock={5}>
      <VStack gap={6}>
        <HStack gap={4} hAlign="between" vAlign="end" wrap="wrap">
          <VStack gap={1}>
            <span className="eyebrow">{eyebrow}</span>
            {/* The one h1 per page; the app name in the shell is a landmark, not a heading. */}
            <Heading level={1}>{title}</Heading>
            {description && (
              <Text type="body" color="secondary">
                {description}
              </Text>
            )}
          </VStack>
          {action}
        </HStack>
        {children}
      </VStack>
    </Section>
  );
}

export function Loading({ label = "Loading" }: { label?: string }) {
  return (
    <Center minHeight={200}>
      <HStack gap={3} vAlign="center">
        <Spinner size="md" />
        <Text type="body" color="secondary">
          {label}…
        </Text>
      </HStack>
    </Center>
  );
}

export function ErrorState({ error, onRetry }: { error: unknown; onRetry?: () => void }) {
  const message = error instanceof Error ? error.message : "Something went wrong";
  return (
    <Banner
      status="error"
      title="Request failed"
      description={message}
      endContent={
        onRetry ? <Button label="Retry" size="sm" variant="secondary" onClick={onRetry} /> : undefined
      }
    />
  );
}

export function Empty({
  title,
  description,
  action,
}: {
  title: string;
  description?: string;
  action?: ReactNode;
}) {
  return <EmptyState title={title} description={description} actions={action} />;
}

/**
 * Query-state gate. Keeps every page from re-implementing the
 * loading / error / empty triad by hand.
 */
export function Query<T>({
  query,
  empty,
  children,
}: {
  query: { isLoading: boolean; error: unknown; data: T | undefined; refetch: () => unknown };
  empty?: ReactNode;
  children: (data: T) => ReactNode;
}) {
  if (query.isLoading) return <Loading />;
  if (query.error) return <ErrorState error={query.error} onRetry={() => query.refetch()} />;
  if (!query.data) return null;
  if (empty && Array.isArray(query.data) && query.data.length === 0) return <>{empty}</>;
  return <>{children(query.data)}</>;
}
