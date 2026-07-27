/**
 * Estate Overview.
 *
 * Stage 4 replaces this placeholder with the virtualized TanStack Table grid: one row per instance,
 * with the two-part backup status (backup succeeded / verified) as separate columns. Those two are
 * deliberately never collapsed into a single indicator — a backup that succeeded but failed
 * verification is the most dangerous state in the product, and merging it with "backed up" would
 * hide exactly the thing an operator needs to see.
 */
export function Estate() {
  return (
    <div className="p-8 max-w-4xl">
      <h1 className="text-xl font-semibold tracking-tight">Estate</h1>
      <p className="text-sm text-(--color-content-muted) mt-1">
        Every database instance Fleetward manages, with live health and backup status.
      </p>

      <div className="mt-8 rounded-lg border border-(--color-border-subtle) p-8 text-center">
        <p className="text-sm text-(--color-content-muted)">
          No instances yet.
        </p>
        <p className="text-sm text-(--color-content-muted) mt-2">
          The add-instance flow arrives in Stage 1 along with the PostgreSQL plugin. Until then, the
          control plane runs and reports its own health under{" "}
          <span className="font-medium text-(--color-content)">System status</span>.
        </p>
      </div>
    </div>
  );
}
