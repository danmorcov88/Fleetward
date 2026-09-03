import { useQuery } from "@tanstack/react-query";

import { api } from "@/lib/api";
import { EstateTable } from "@/estate/EstateTable";
import { join } from "@/estate/row";
import { severity } from "@/estate/status";

/**
 * Estate Overview.
 *
 * The screen the product exists for: fifty servers, and the two questions that matter answerable
 * without clicking anything — did this server's backup run when it was supposed to, and is that
 * backup known to be restorable.
 *
 * The refresh is a poll, not a stream. Fifty rows on a thirty-second cadence is a polling problem,
 * and a server-streaming RPC could not be served over the gateway anyway (ADR-0019). The promise is
 * that the answer is never more than about thirty seconds stale, and this is one of the three
 * things that deliver it — the others are the `discovery` schedule that keeps health moving, and
 * alert delivery, which is what makes it monitoring rather than a dashboard, and is not built yet.
 */
const REFRESH_MS = 30_000;

export function Estate() {
  const adherence = useQuery({
    queryKey: ["backup-adherence"],
    queryFn: api.backupAdherence,
    refetchInterval: REFRESH_MS,
  });

  const instances = useQuery({
    queryKey: ["instances"],
    queryFn: api.instances,
    refetchInterval: REFRESH_MS,
  });

  const rows = join(adherence.data?.instances, instances.data?.instances);
  const attention = rows.filter((r) => severity(r) <= 2).length;
  const pending = adherence.isPending || instances.isPending;
  const error = adherence.error ?? instances.error;

  return (
    <div className="p-8 max-w-6xl">
      <div className="flex items-baseline justify-between gap-4 flex-wrap">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">Estate</h1>
          <p className="text-sm text-(--color-content-muted) mt-1">
            Every database instance Fleetward knows about, with its health and the two-part backup
            status.
          </p>
        </div>
        <p className="text-xs text-(--color-content-muted) tabular-nums">
          {rows.length} instance{rows.length === 1 ? "" : "s"}
          {attention > 0 ? ` · ${attention} needing attention` : ""} · refreshed every 30s
        </p>
      </div>

      <section className="mt-8">
        {error && (
          <div className="rounded-lg border border-(--color-status-critical) p-4 mb-4">
            <p className="text-sm font-medium">The estate could not be read.</p>
            <p className="text-sm text-(--color-content-muted) mt-1">{(error as Error).message}</p>
          </div>
        )}

        {pending && !error && (
          <p className="text-sm text-(--color-content-muted)">Reading the estate…</p>
        )}

        {!pending && !error && rows.length === 0 && (
          <div className="rounded-lg border border-(--color-border-subtle) p-8 text-center">
            <p className="text-sm text-(--color-content-muted)">No instances yet.</p>
            <p className="text-sm text-(--color-content-muted) mt-2">
              Add one with{" "}
              <code className="font-medium text-(--color-content)">
                fleetward-cli instance add
              </code>
              . Adding an instance from this screen is not built yet.
            </p>
          </div>
        )}

        {rows.length > 0 && <EstateTable rows={rows} />}
      </section>
    </div>
  );
}
