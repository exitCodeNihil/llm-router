import { useState } from "react";
import { plural } from "../lib/format";
import { useDeployments } from "../lib/hooks";
import { Tooltip } from "@astryxdesign/core/Tooltip";
import { MultiSelector } from "@astryxdesign/core/MultiSelector";
import { HStack, VStack } from "@astryxdesign/core/Stack";
import { Grid } from "@astryxdesign/core/Grid";
import { TextInput } from "@astryxdesign/core/TextInput";
import { Selector } from "@astryxdesign/core/Selector";
import { Token } from "@astryxdesign/core/Token";
import { Text } from "@astryxdesign/core/Text";
import { Button } from "@astryxdesign/core/Button";
import type { BudgetPeriod } from "../lib/types";

// ── Budget / rate-limit fields, shared by keys, users and teams ──
export interface LimitsState {
  budget_usd: string;
  budget_period: BudgetPeriod;
  rpm_limit: string;
  tpm_limit: string;
}

export const emptyLimits: LimitsState = {
  budget_usd: "",
  budget_period: "monthly",
  rpm_limit: "",
  tpm_limit: "",
};

export function limitsFrom(o: {
  budget_usd: number | null;
  budget_period: BudgetPeriod | null;
  rpm_limit: number | null;
  tpm_limit: number | null;
}): LimitsState {
  // Nullable, not falsy: a $0 budget is a real, enforced cap.
  return {
    budget_usd: o.budget_usd == null ? "" : String(o.budget_usd),
    budget_period: o.budget_period ?? "monthly",
    rpm_limit: o.rpm_limit == null ? "" : String(o.rpm_limit),
    tpm_limit: o.tpm_limit == null ? "" : String(o.tpm_limit),
  };
}

/** True when a budget string is present but not a number ("12.34.56"). */
export function limitsInvalid(s: LimitsState): boolean {
  return s.budget_usd !== "" && Number.isNaN(Number(s.budget_usd));
}

/** Blank strings clear the limit server-side, so send explicit nulls. */
export function limitsPayload(s: LimitsState) {
  return {
    budget_usd: s.budget_usd === "" ? null : Number(s.budget_usd),
    budget_period: s.budget_usd === "" ? null : s.budget_period,
    rpm_limit: s.rpm_limit === "" ? null : Number(s.rpm_limit),
    tpm_limit: s.tpm_limit === "" ? null : Number(s.tpm_limit),
  };
}

export function LimitsFields({
  value,
  onChange,
}: {
  value: LimitsState;
  onChange: (v: LimitsState) => void;
}) {
  const set = (patch: Partial<LimitsState>) => onChange({ ...value, ...patch });
  return (
    <VStack gap={3}>
      <Grid columns={{ minWidth: 150, repeat: "fit" }} gap={3}>
        <TextInput
          label="Budget (USD)"
          placeholder="Unlimited"
          value={value.budget_usd}
          onChange={(v) => set({ budget_usd: v.replace(/[^0-9.]/g, "") })}
          description="Blank = no cap; 0 blocks every request"
          status={
            limitsInvalid(value)
              ? { type: "error", message: "Enter a number" }
              : undefined
          }
        />
        <Selector
          label="Period"
          value={value.budget_period}
          onChange={(v) => set({ budget_period: v as BudgetPeriod })}
          isDisabled={value.budget_usd === ""}
          options={[
            { value: "daily", label: "Daily" },
            { value: "monthly", label: "Monthly" },
            { value: "total", label: "Total" },
          ]}
        />
      </Grid>
      <Grid columns={{ minWidth: 150, repeat: "fit" }} gap={3}>
        <TextInput
          label="Requests / min"
          placeholder="Unlimited"
          value={value.rpm_limit}
          onChange={(v) => set({ rpm_limit: v.replace(/[^0-9]/g, "") })}
        />
        <TextInput
          label="Tokens / min"
          placeholder="Unlimited"
          value={value.tpm_limit}
          onChange={(v) => set({ tpm_limit: v.replace(/[^0-9]/g, "") })}
        />
      </Grid>
    </VStack>
  );
}

// ── Tag editor ──────────────────────────────────────────────────
export function TagsInput({
  value,
  onChange,
  label = "Tags",
  description,
}: {
  value: string[];
  onChange: (v: string[]) => void;
  label?: string;
  description?: string;
}) {
  const [draft, setDraft] = useState("");

  const add = () => {
    const t = draft.trim().replace(/,$/, "");
    if (t && !value.includes(t)) onChange([...value, t]);
    setDraft("");
  };

  return (
    <VStack gap={2}>
      <HStack gap={2} vAlign="end">
        <TextInput
          label={label}
          description={description}
          placeholder="Add a tag and press Enter"
          value={draft}
          onChange={setDraft}
          onKeyDown={(e) => {
            if (e.key === "Enter" || e.key === ",") {
              e.preventDefault();
              add();
            }
            // Backspace on an empty field removes the last tag — standard
            // chip-input behaviour users expect.
            if (e.key === "Backspace" && !draft && value.length) {
              onChange(value.slice(0, -1));
            }
          }}
        />
        <Button
          label="Add"
          variant="secondary"
          onClick={add}
          isDisabled={!draft.trim()}
        />
      </HStack>
      {value.length > 0 && (
        <HStack gap={1.5} wrap="wrap">
          {value.map((t) => (
            <Token
              key={t}
              label={t}
              onRemove={() => onChange(value.filter((x) => x !== t))}
            />
          ))}
        </HStack>
      )}
    </VStack>
  );
}

export function TagChips({ tags }: { tags: string[] | null }) {
  if (!tags?.length)
    return (
      <Text type="supporting" color="secondary">
        —
      </Text>
    );
  return (
    <HStack gap={1} wrap="wrap">
      {tags.map((t) => (
        <Token key={t} label={t} size="sm" />
      ))}
    </HStack>
  );
}

// ── Model policy ────────────────────────────────────────────────
/**
 * Picker for a user's or team's model policy. Empty = every model. The list
 * comes from the routing table, so only admins (who can edit users and teams
 * anyway) get real options.
 */
export function ModelPolicyField({
  value,
  onChange,
  subject,
}: {
  value: string[];
  onChange: (v: string[]) => void;
  subject: "user" | "team";
}) {
  const deployments = useDeployments();
  const aliases = [
    ...new Set((deployments.data ?? []).map((d) => d.model_name)),
  ].sort();
  return (
    <MultiSelector
      label="Allowed models"
      value={value}
      onChange={onChange}
      options={aliases.map((a) => ({ value: a, label: a }))}
      placeholder="All models"
      triggerDisplay="badges"
      hasSearch={aliases.length > 8}
      hasSelectAll
      description={
        subject === "team"
          ? "Every key owned by this team is limited to these, whatever the key itself allows."
          : "Every key this person owns is limited to these, whatever the key itself allows."
      }
    />
  );
}

/** Table cell for a model list: "All", or the count with the names on hover. */
export function ModelsCell({
  models,
}: {
  models: string[] | null | undefined;
}) {
  if (!models || models.length === 0) {
    return (
      <Text type="supporting" color="secondary">
        All
      </Text>
    );
  }
  return (
    <Tooltip content={models.join(", ")}>
      <Text type="supporting">{plural(models.length, "model")}</Text>
    </Tooltip>
  );
}
