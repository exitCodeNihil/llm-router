import { type ReactNode, useRef, useState } from "react";
import { Dialog, DialogHeader } from "@astryxdesign/core/Dialog";
import { AlertDialog } from "@astryxdesign/core/AlertDialog";
import { HStack, VStack } from "@astryxdesign/core/Stack";
import { Button } from "@astryxdesign/core/Button";
import { Text } from "@astryxdesign/core/Text";
import { Banner } from "@astryxdesign/core/Banner";
import { CodeBlock } from "@astryxdesign/core/CodeBlock";
import { useToast } from "@astryxdesign/core/Toast";

/**
 * Form dialog. `purpose="form"` disables backdrop-click dismissal after the
 * user has interacted, so a stray click can't discard a half-filled form.
 * Focus trapping, Escape, scroll lock and focus restore are handled by Astryx.
 */
export function FormDialog({
  isOpen,
  onClose,
  title,
  subtitle,
  wide,
  children,
}: {
  isOpen: boolean;
  onClose: () => void;
  title: string;
  subtitle?: string;
  wide?: boolean;
  children: ReactNode;
}) {
  return (
    <Dialog
      isOpen={isOpen}
      onOpenChange={(o) => !o && onClose()}
      purpose="form"
      width={wide ? 680 : 460}
      // Long forms must scroll inside the dialog, never push Cancel/Save
      // below the viewport.
      maxHeight="calc(100vh - 48px)"
    >
      <DialogHeader title={title} subtitle={subtitle} onOpenChange={(o) => !o && onClose()} />
      {/* The dialog's inner column clips; the body is what scrolls, so the
          Cancel/Save row is always reachable on a short viewport. */}
      <VStack gap={4} padding={4} isScrollable style={{ minHeight: 0 }}>
        {children}
      </VStack>
    </Dialog>
  );
}

/** Right-aligned Cancel / Submit pair used by every form dialog. */
export function FormActions({
  onCancel,
  submitLabel,
  isLoading,
  isDisabled,
}: {
  onCancel: () => void;
  submitLabel: string;
  isLoading?: boolean;
  isDisabled?: boolean;
}) {
  return (
    <HStack gap={2} hAlign="end">
      <Button label="Cancel" variant="ghost" onClick={onCancel} />
      <Button
        label={submitLabel}
        variant="primary"
        type="submit"
        isLoading={isLoading}
        isDisabled={isDisabled}
      />
    </HStack>
  );
}

// Generic confirmation for any consequential action, not just deletes:
// enabling or disabling a key, deployment, user or token issuer all change live
// behaviour on one click, so each states its consequence before proceeding.
export function Confirm({
  target,
  title,
  description,
  actionLabel = "Delete",
  isLoading,
  onCancel,
  onConfirm,
}: {
  target: unknown;
  title: string;
  description: string;
  actionLabel?: string;
  isLoading?: boolean;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  // The target is nulled before the close animation ends; keep the last
  // wording so "Disable key" does not flip to "Enable key" on the way out.
  const last = useRef({ title, description, actionLabel });
  if (target) last.current = { title, description, actionLabel };
  return (
    <AlertDialog
      isOpen={!!target}
      onOpenChange={(o) => !o && onCancel()}
      title={last.current.title}
      description={last.current.description}
      actionLabel={last.current.actionLabel}
      isActionLoading={isLoading}
      onAction={onConfirm}
    />
  );
}

/**
 * One-time secret reveal. The gateway returns a key's plaintext exactly once,
 * so this is deliberately `purpose="required"` — no Escape, no backdrop
 * dismissal — forcing an explicit acknowledgement before it is lost.
 */
export function SecretDialog({
  secret,
  onClose,
  title = "API key created",
  noun = "key",
}: {
  secret: string;
  onClose: () => void;
  title?: string;
  /** What the secret is, for the copy ("key", "node token"). */
  noun?: string;
}) {
  const showToast = useToast();
  const [copied, setCopied] = useState(false);

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(secret);
      setCopied(true);
      showToast({ body: `Copied the ${noun} to the clipboard`, uniqueID: "secret-copy" });
    } catch {
      // navigator.clipboard is undefined on plain-HTTP non-localhost origins,
      // which is a realistic self-hosted deployment. Fail loudly, not silently.
      showToast({
        body: `Copy failed — select the ${noun} and copy manually`,
        type: "error",
        uniqueID: "secret-copy",
      });
    }
  };

  return (
    <Dialog isOpen onOpenChange={() => {}} purpose="required" width={560} maxHeight="calc(100vh - 48px)">
      <DialogHeader title={title} subtitle={`This is the only time the full ${noun} is shown.`} />
      <VStack gap={4} padding={4} isScrollable style={{ minHeight: 0 }}>
        <Banner
          status="warning"
          title="Copy it now"
          description={`The gateway stores only a hash. If you lose this value you must issue a new ${noun}.`}
        />
        <CodeBlock code={secret} language="plaintext" width="100%" isWrapped hasCopyButton={false} />
        <HStack gap={2} hAlign="end">
          <Button label={copied ? "Copied" : `Copy ${noun}`} variant="secondary" onClick={copy} />
          <Button label="Done" variant="primary" onClick={onClose} />
        </HStack>
      </VStack>
    </Dialog>
  );
}

export function Hint({ children }: { children: ReactNode }) {
  return (
    <Text type="supporting" color="secondary">
      {children}
    </Text>
  );
}
