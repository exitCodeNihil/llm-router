import { useEffect, useMemo, useState } from "react";
import { useTableSortable, useTableSortableState } from "@astryxdesign/core/Table";
import type { TablePlugin, TableSortState } from "@astryxdesign/core/Table";
import { TextInput } from "@astryxdesign/core/TextInput";
import { Button } from "@astryxdesign/core/Button";
import { Center } from "@astryxdesign/core/Center";
import { VStack } from "@astryxdesign/core/Stack";

/**
 * Search + click-to-sort for every list page, so each table behaves the same
 * way. Search is a substring match over whatever `search` renders for a row;
 * sorting comes from the design system's plugin and columns opt in with
 * `sortable: true`.
 */
export function useTableTools<T extends Record<string, unknown>>(
  data: T[] | undefined,
  opts: { search: (row: T) => string; defaultSort?: TableSortState; sort?: boolean },
) {
  const [query, setQuery] = useState("");
  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    const all = data ?? [];
    return q ? all.filter((r) => opts.search(r).toLowerCase().includes(q)) : all;
    // opts.search is a stable module-level function on every page
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [data, query]);
  const sortable = useTableSortableState<T>({
    data: filtered,
    defaultSort: opts.defaultSort,
    allowUnsortedState: true,
  });
  const plugin = useTableSortable<T>(sortable.sortConfig);
  const plugins: Record<string, TablePlugin<T>> = opts.sort === false ? {} : { sortable: plugin };
  return {
    query,
    setQuery,
    rows: opts.sort === false ? filtered : sortable.sortedData,
    plugins,
    total: (data ?? []).length,
  };
}

export function SearchBox({
  value,
  onChange,
  placeholder = "Search…",
}: {
  value: string;
  onChange: (v: string) => void;
  placeholder?: string;
}) {
  return (
    <TextInput
      label="Search"
      isLabelHidden
      size="sm"
      value={value}
      onChange={onChange}
      placeholder={placeholder}
      hasClear
      width={220}
    />
  );
}

/** The search text typed so far, and the trimmed value to query the server with once typing pauses. */
export function useServerSearch(delayMs = 300) {
  const [query, setQuery] = useState("");
  const [q, setQ] = useState("");
  useEffect(() => {
    const t = setTimeout(() => setQ(query.trim()), delayMs);
    return () => clearTimeout(t);
  }, [query, delayMs]);
  return { query, setQuery, q };
}

/** Footer of a paged table. */
export function LoadMore({
  hasNextPage,
  isFetchingNextPage,
  fetchNextPage,
}: {
  hasNextPage: boolean;
  isFetchingNextPage: boolean;
  fetchNextPage: () => void;
}) {
  if (!hasNextPage) return null;
  return (
    <Center>
      <VStack padding={4}>
        <Button
          label={isFetchingNextPage ? "Loading…" : "Load more"}
          variant="secondary"
          isLoading={isFetchingNextPage}
          onClick={() => fetchNextPage()}
        />
      </VStack>
    </Center>
  );
}
