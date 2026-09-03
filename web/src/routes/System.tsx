import { useQuery } from "@tanstack/react-query";
import { api, type ComponentStatus } from "@/lib/api";
import { StatusDot, type StatusTone } from "@/components/StatusDot";

function toneFor(status: string): StatusTone {
  switch (status) {
    case "healthy":
      return "ok";
    case "degraded":
      return "warn";
    case "unhealthy":
      return "critical";
    default:
      return "unknown";
  }
}

/**
 * System status renders the control plane's own readiness.
 *
 * It was the first screen built, because it is the one that proves the whole stack is wired
 * together: browser to Vite proxy to control plane to Postgres, MinIO, VictoriaMetrics, and the
 * plugin manager.
 */
export function System() {
  const readiness = useQuery({
    queryKey: ["readiness"],
    queryFn: api.readiness,
    refetchInterval: 5_000,
  });

  const version = useQuery({ queryKey: ["version"], queryFn: api.version });

  return (
    <div className="p-8 max-w-3xl">
      <h1 className="text-xl font-semibold tracking-tight">System status</h1>
      <p className="text-sm text-(--color-content-muted) mt-1">
        Health of the control plane and its dependencies.
      </p>

      <section className="mt-8">
        {readiness.isPending && (
          <p className="text-sm text-(--color-content-muted)">Checking…</p>
        )}

        {readiness.isError && (
          <div className="rounded-lg border border-(--color-status-critical) p-4">
            <StatusDot tone="critical" label="Control plane unreachable" />
            <p className="text-sm text-(--color-content-muted) mt-2">
              {(readiness.error as Error).message}
            </p>
          </div>
        )}

        {readiness.data && (
          <div className="rounded-lg border border-(--color-border-subtle) overflow-hidden">
            <div className="px-4 py-3 border-b border-(--color-border-subtle) bg-(--color-surface-muted)">
              <StatusDot
                tone={toneFor(readiness.data.status)}
                label={`Control plane: ${readiness.data.status}`}
              />
            </div>
            <ul>
              {readiness.data.components?.map((component: ComponentStatus) => (
                <li
                  key={component.name}
                  className="px-4 py-3 border-b border-(--color-border-subtle) last:border-b-0 flex items-baseline gap-4"
                >
                  <span className="w-28 shrink-0">
                    <StatusDot tone={toneFor(component.status)} label={component.name} />
                  </span>
                  <span className="text-xs text-(--color-content-muted) tabular-nums w-16">
                    {component.latency_ms}ms
                  </span>
                  <span className="text-xs text-(--color-content-muted) min-w-0 break-words">
                    {component.error ?? (component.critical ? "required" : "optional")}
                  </span>
                </li>
              ))}
            </ul>
          </div>
        )}
      </section>

      {version.data && (
        <dl className="mt-8 grid grid-cols-[8rem_1fr] gap-y-1 text-sm">
          <dt className="text-(--color-content-muted)">Version</dt>
          <dd className="tabular-nums">{version.data.version}</dd>
          <dt className="text-(--color-content-muted)">Commit</dt>
          <dd className="tabular-nums">{version.data.commit}</dd>
          <dt className="text-(--color-content-muted)">Contract</dt>
          <dd className="tabular-nums">{version.data.contract_version}</dd>
          {/* This row used to read `version.data.platform`, a field GetVersionResponse has never
              carried, so it rendered blank from the day it was written. Nothing caught it because
              the type it was checked against was hand-written from the same wrong assumption. It is
              the first thing the generated types found. */}
          <dt className="text-(--color-content-muted)">Go</dt>
          <dd className="tabular-nums">{version.data.go_version}</dd>
          <dt className="text-(--color-content-muted)">Built</dt>
          <dd className="tabular-nums">{version.data.build_date}</dd>
        </dl>
      )}
    </div>
  );
}
