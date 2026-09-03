import { createColumnHelper, type CellData, type ColumnDef } from "@tanstack/react-table";

import { Badge } from "@/components/Badge";
import { StatusDot } from "@/components/StatusDot";
import { age, utc } from "@/lib/format";
import { estateFeatures } from "./features";
import type { EstateRow } from "./row";
import { adherenceCell, healthCell, verificationCell } from "./status";

const helper = createColumnHelper<typeof estateFeatures, EstateRow>();

/**
 * The four columns of the estate view.
 *
 * Five facts compete for a row — health, when the last backup was, who took it, whether it was
 * verified, and whether the instance is adherent — and five columns is not a glance. What was
 * collapsed and what was not is the whole design:
 *
 *   - Adherence and the last backup's time are one cell, because the verdict and the evidence for
 *     it are one thought.
 *   - Origin has no column of its own. For a reader scanning fifty rows it has exactly one
 *     consequence — whether a verification is possible at all — so it is folded into the Verified
 *     cell, where it reads "n/a — not ours" and cannot be read apart from the verdict. A separate
 *     column would let someone read Verified alone and misunderstand a blank (ADR-0015).
 *   - Everything that weakens an answer — the caveats B3 attaches, the declared schedule — is
 *     behind the row rather than in a column.
 */
export const columns: ColumnDef<typeof estateFeatures, EstateRow>[] = [
  helper.accessor((row): CellData => row.instance_name ?? "", {
    id: "instance",
    header: "Instance",
    cell: (ctx) => {
      const row = ctx.row.original;
      return (
        <div className="min-w-0">
          <div className="font-medium truncate">{row.instance_name}</div>
          <div className="text-xs text-(--color-content-muted) truncate">
            {row.engine_type}
            {row.instance?.host ? ` · ${row.instance.host}:${row.instance.port}` : ""}
          </div>
        </div>
      );
    },
  }),

  helper.accessor((row): CellData => row.instance?.health ?? "", {
    id: "health",
    header: "Health",
    cell: (ctx) => {
      const instance = ctx.row.original.instance;
      const cell = healthCell(instance?.health);
      return (
        <div className="min-w-0">
          <StatusDot tone={cell.tone} label={cell.label} />
          {/* The age is not decoration. "healthy" is reassuring; "healthy · 3 weeks ago" is the
              same word telling the truth, and without a discovery schedule running it is the only
              thing that stops this column from being a comfortable lie. */}
          <div className="text-xs text-(--color-content-muted) mt-0.5">
            {instance?.last_seen_at ? age(instance.last_seen_at) : "never reached"}
          </div>
        </div>
      );
    },
  }),

  helper.accessor((row): CellData => row.state ?? "", {
    id: "backup",
    header: "Backup",
    cell: (ctx) => {
      const row = ctx.row.original;
      const cell = adherenceCell(row.state);
      const last = row.satisfied_by ?? row.latest_backup;
      return (
        <div className="min-w-0">
          <Badge tone={cell.tone} label={cell.label} title={cell.detail} />
          <div className="text-xs text-(--color-content-muted) mt-1">
            {last?.completed_at ? `last ${age(last.completed_at)}` : "no backup on record"}
          </div>
        </div>
      );
    },
  }),

  helper.accessor(
    (row): CellData => verificationCell(row.satisfied_by ?? row.latest_backup).label,
    {
      id: "verified",
      header: "Verified",
      cell: (ctx) => {
        const row = ctx.row.original;
        const backup = row.satisfied_by ?? row.latest_backup;
        const cell = verificationCell(backup);
        return (
          <div className="min-w-0">
            <Badge tone={cell.tone} label={cell.label} title={cell.detail} />
            <div className="text-xs text-(--color-content-muted) mt-1">
              {backup?.verification?.completed_at
                ? utc(backup.verification.completed_at)
                : ""}
            </div>
          </div>
        );
      },
    },
  ),
];
