import { useMemo, useState } from "react";
import { useTableSortable, useTableSortableState } from "@astryxdesign/core/Table";
import type { TablePlugin, TableSortState } from "@astryxdesign/core/Table";
import { TextInput } from "@astryxdesign/core/TextInput";

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
