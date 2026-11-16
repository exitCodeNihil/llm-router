import { useMemo, useState } from "react";
import { Typeahead } from "@astryxdesign/core/Typeahead";
import type { SearchableItem } from "@astryxdesign/core/Typeahead";
import { VStack } from "@astryxdesign/core/Stack";
import { Text } from "@astryxdesign/core/Text";

import { api } from "../lib/api";
import { useLookup } from "../lib/hooks";
import type { ApiKey, User } from "../lib/types";

type Item = SearchableItem<{ detail?: string }>;
const PAGE = 20;

const SOURCES = {
  user: {
    path: "/api/users",
    toItem: (u: User): Item => ({ id: u.id, label: u.email, auxiliaryData: { detail: u.name } }),
  },
  key: {
    path: "/api/keys",
    toItem: (k: ApiKey): Item => ({
      id: k.id,
      label: k.name || k.key_prefix,
      auxiliaryData: { detail: [k.key_prefix, k.user_email ?? k.team_name].filter(Boolean).join(" · ") },
    }),
  },
};

/**
 * Single-select picker for one user or key, searched on the server so it
 * never loads the whole list. `value` is an id ("" = none); when only the id is
 * known its label comes from /api/lookup. Pass `excludeFromTeam` to leave out a
 * team's members (users only).
 */
export function EntityPicker({
  kind,
  value,
  onChange,
  label,
  description,
  placeholder,
  isRequired,
  isDisabled,
  isLabelHidden,
  hasClear = true,
  size,
  width,
  excludeFromTeam,
}: {
  kind: "user" | "key";
  value: string;
  onChange: (id: string, label?: string) => void;
  label: string;
  description?: string;
  placeholder?: string;
  isRequired?: boolean;
  isDisabled?: boolean;
  isLabelHidden?: boolean;
  hasClear?: boolean;
  size?: "sm" | "md" | "lg";
  width?: number | string;
  excludeFromTeam?: string;
}) {
  // What the user picked this session; an id set from outside is looked up.
  const [picked, setPicked] = useState<Item | null>(null);
  const known = picked && picked.id === value ? picked : null;
  const lookup = useLookup({
    users: kind === "user" && value && !known ? [value] : [],
    keys: kind === "key" && value && !known ? [value] : [],
  });

  const source = useMemo(() => {
    const { path, toItem } = SOURCES[kind];
    const fetchItems = async (q: string): Promise<Item[]> => {
      const p = new URLSearchParams({ limit: String(PAGE) });
      if (q.trim()) p.set("q", q.trim());
      if (excludeFromTeam) p.set("not_in_team", excludeFromTeam);
      const r = await api.get<{ items: Array<User & ApiKey> }>(`${path}?${p}`);
      return (r.items ?? []).map(toItem);
    };
    return { search: fetchItems, bootstrap: () => fetchItems("") };
  }, [kind, excludeFromTeam]);

  const text = kind === "user" ? lookup.data?.users[value] : lookup.data?.keys[value];
  const current: Item | null = !value
    ? null
    : (known ?? { id: value, label: text ?? (lookup.isLoading ? "Loading…" : value) });

  return (
    <Typeahead
      label={label}
      isLabelHidden={isLabelHidden}
      description={description}
      placeholder={placeholder}
      isRequired={isRequired}
      isDisabled={isDisabled}
      hasClear={hasClear}
      size={size}
      width={width}
      value={current}
      onChange={(item) => {
        setPicked(item);
        onChange(item?.id ?? "", item?.label);
      }}
      searchSource={source}
      debounceMs={300}
      hasEntriesOnFocus
      emptySearchResultsText={kind === "user" ? "No matching users" : "No matching keys"}
      renderItem={(item) => (
        <VStack gap={0}>
          <Text type="body">{item.label}</Text>
          {item.auxiliaryData?.detail && (
            <Text type="supporting" color="secondary">
              {item.auxiliaryData.detail}
            </Text>
          )}
        </VStack>
      )}
    />
  );
}
