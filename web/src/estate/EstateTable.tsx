import { useState } from "react";
import { useTable, type SortingState } from "@tanstack/react-table";

import { Badge } from "@/components/Badge";
import { grace, utc } from "@/lib/format";
import { columns } from "./columns";
import { estateFeatures } from "./features";
import type { EstateRow } from "./row";

/**
 * The estate grid.
 *
 * Not virtualized, and that is a decision rather than an omission. The product's stated scale is an
 * estate of about fifty servers; windowing fifty rows adds a dependency and a class of scrolling
 * bug in exchange for nothing measurable. ADR-0010 says TanStack Table "virtualizes the estate grid
 * without us writing windowing logic", which is not accurate — virtualization is a separate package
 * — and the row count is what settles it either way.
 *
 * Rows arrive already ordered by severity (see `join`). Clicking a header sorts by that column
 * instead, which is the escape hatch for the reader who wants a different question answered; the
 * screen opens on the one it exists for.
 */
export function EstateTable({ rows }: { rows: EstateRow[] }) {
  const [sorting, setSorting] = useState<SortingState>([]);
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});

  const table = useTable({
    features: estateFeatures,
    columns,
    data: rows,
    state: { sorting },
    onSortingChange: setSorting,
    getRowId: (row) => row.instance_id ?? row.instance_name ?? "",
  });

  return (
    <div className="rounded-lg border border-(--color-border-subtle) overflow-x-auto">
      <table className="w-full text-sm">
        <thead className="bg-(--color-surface-muted)">
          {table.getHeaderGroups().map((group) => (
            <tr key={group.id}>
              {group.headers.map((header) => (
                <th
                  key={header.id}
                  scope="col"
                  className="text-left font-medium text-xs uppercase tracking-wide text-(--color-content-muted) px-4 py-2 border-b border-(--color-border-subtle)"
                >
                  <button
                    type="button"
                    className="inline-flex items-center gap-1 hover:text-(--color-content)"
                    onClick={header.column.getToggleSortingHandler()}
                  >
                    <table.FlexRender header={header} />
                    {{ asc: "▲", desc: "▼" }[header.column.getIsSorted() as string] ?? ""}
                  </button>
                </th>
              ))}
              <th scope="col" className="w-8 border-b border-(--color-border-subtle)" />
            </tr>
          ))}
        </thead>

        <tbody>
          {table.getRowModel().rows.map((row) => {
            const isOpen = expanded[row.id] ?? false;
            const caveats = row.original.caveats ?? [];
            return (
              <>
                <tr
                  key={row.id}
                  data-testid="estate-row"
                  data-instance={row.original.instance_name}
                  className="border-b border-(--color-border-subtle) last:border-b-0 align-top"
                >
                  {row.getAllCells().map((cell) => (
                    <td key={cell.id} className="px-4 py-3">
                      <table.FlexRender cell={cell} />
                    </td>
                  ))}
                  <td className="px-2 py-3">
                    <button
                      type="button"
                      aria-expanded={isOpen}
                      aria-label={`Details for ${row.original.instance_name}`}
                      onClick={() =>
                        setExpanded((prev) => ({ ...prev, [row.id]: !isOpen }))
                      }
                      className="text-(--color-content-muted) hover:text-(--color-content) px-1"
                    >
                      {isOpen ? "▾" : "▸"}
                      {/* A row whose answer rests on something weaker than a backup Fleetward took
                          says so before it is opened, or the caveat is one nobody reads. */}
                      {caveats.length > 0 && !isOpen && (
                        <span className="ml-1 text-(--color-status-warn)" title={caveats[0]}>
                          !
                        </span>
                      )}
                    </button>
                  </td>
                </tr>

                {isOpen && (
                  <tr
                    key={`${row.id}-detail`}
                    className="border-b border-(--color-border-subtle) bg-(--color-surface-muted)"
                  >
                    <td colSpan={row.getAllCells().length + 1} className="px-4 py-3">
                      <RowDetail row={row.original} />
                    </td>
                  </tr>
                )}
              </>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

/**
 * What is behind a row: the declaration it is being held to, and everything that weakens the
 * answer.
 *
 * The caveats come from the plugin rather than from anything core inferred (ADR-0027). They are
 * sentences a DBA can act on — "this source assigns no identity, so a renamed file looks like a new
 * backup" — and they are the difference between an answer and a confident answer.
 */
function RowDetail({ row }: { row: EstateRow }) {
  const backup = row.satisfied_by ?? row.latest_backup;
  const caveats = row.caveats ?? [];

  return (
    <div className="grid gap-3 md:grid-cols-2">
      <dl className="grid grid-cols-[9rem_1fr] gap-y-1 text-xs">
        <dt className="text-(--color-content-muted)">Expected</dt>
        <dd className="tabular-nums">
          {row.expected_cron ? `${row.expected_cron} (${row.timezone ?? "UTC"})` : "nothing declared"}
        </dd>
        <dt className="text-(--color-content-muted)">Grace</dt>
        <dd className="tabular-nums">{grace(row.expected_grace_minutes)}</dd>
        <dt className="text-(--color-content-muted)">Window closed</dt>
        <dd className="tabular-nums">{utc(row.deadline)}</dd>
        <dt className="text-(--color-content-muted)">Last backup</dt>
        <dd className="tabular-nums">{utc(backup?.completed_at)}</dd>
        <dt className="text-(--color-content-muted)">Origin</dt>
        <dd>
          {backup?.origin === "BACKUP_ORIGIN_OBSERVED"
            ? "observed — somebody else took it"
            : backup
              ? "managed — Fleetward took it"
              : "—"}
        </dd>
      </dl>

      <div className="text-xs">
        {caveats.length === 0 ? (
          <p className="text-(--color-content-muted)">
            Nothing weakens this answer: it rests on a backup Fleetward took itself.
          </p>
        ) : (
          <ul className="space-y-1">
            {caveats.map((caveat) => (
              <li key={caveat} className="flex gap-2">
                <Badge tone="warn" label="caveat" />
                <span className="text-(--color-content-muted)">{caveat}</span>
              </li>
            ))}
          </ul>
        )}
      </div>
    </div>
  );
}
