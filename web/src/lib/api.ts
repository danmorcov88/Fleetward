/**
 * Thin client for the control plane REST API.
 *
 * The response types are generated. `api.gen.ts` comes from `api/openapi/openapi.yaml`, which comes
 * from the Protobuf contract, so a field that is renamed or removed in the `.proto` fails
 * `npm run build` rather than showing up as an empty column at three in the morning. Run
 * `npm run generate` after `make proto`; CI regenerates and diffs both, so a stale one fails the
 * same way stale protobuf output does.
 *
 * Two things here are hand-written on purpose, and they are the two the contract does not describe
 * (ADR-0029):
 *
 *   - `/readyz` is a hand-written handler on the HTTP server rather than a gRPC method, so it never
 *     appears in the document. Its types live below.
 *   - Errors are RFC 9457 problem details, produced by the gateway's error handler. The generated
 *     document claims a `google.rpc.Status`, because that is what the OpenAPI generator writes for
 *     every gRPC service regardless of how the errors are actually rendered.
 *
 * Both are written down rather than papered over. Generating from a document that describes
 * something else is how the previous version of this file came to be confidently wrong.
 */

import type { components } from "./api.gen";

type Schemas = components["schemas"];

export type Instance = Schemas["Instance"];
export type ListInstancesResponse = Schemas["ListInstancesResponse"];
export type InstanceAdherence = Schemas["InstanceAdherence"];
export type GetBackupAdherenceResponse = Schemas["GetBackupAdherenceResponse"];
export type Backup = Schemas["Backup"];
export type Verification = Schemas["Verification"];
export type VersionInfo = Schemas["GetVersionResponse"];

export type AdherenceState = NonNullable<InstanceAdherence["state"]>;
export type BackupOrigin = NonNullable<Backup["origin"]>;
export type VerificationStatus = NonNullable<Verification["status"]>;
export type HealthState = NonNullable<Instance["health"]>;

/** ComponentStatus and ReadinessStatus describe `/readyz`, which is outside the contract. */
export interface ComponentStatus {
  name: string;
  status: "healthy" | "degraded" | "unhealthy";
  critical: boolean;
  error?: string;
  latency_ms: number;
}

export interface ReadinessStatus {
  status: "healthy" | "degraded" | "unhealthy";
  components?: ComponentStatus[];
  checked_at: string;
}

/** Problem is the single error shape every control plane error uses (RFC 9457). */
export interface Problem {
  type: string;
  title: string;
  status: number;
  detail?: string;
  request_id?: string;
}

export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly problem?: Problem,
  ) {
    super(problem?.detail ?? problem?.title ?? `request failed with status ${status}`);
    this.name = "ApiError";
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(path, {
    ...init,
    headers: { Accept: "application/json", ...init?.headers },
  });

  if (!response.ok) {
    // A readiness probe answers 503 with a perfectly good body, so parse before deciding to throw.
    let problem: Problem | undefined;
    try {
      problem = (await response.clone().json()) as Problem;
    } catch {
      problem = undefined;
    }
    if (problem?.title) {
      throw new ApiError(response.status, problem);
    }
  }

  return (await response.json()) as T;
}

export const api = {
  readiness: () => request<ReadinessStatus>("/readyz"),
  version: () => request<VersionInfo>("/api/v1/version"),

  /**
   * The estate view's two reads.
   *
   * Two rather than one, because health belongs to an instance and adherence belongs to a backup,
   * and putting either on the other's endpoint would be a contract shaped by one screen. They are
   * joined in the browser on `instance_id`, and both refetch on the same interval, so the two
   * halves of a row can never be more than one tick apart.
   *
   * `page_size` is deliberately generous. The product's stated scale is an estate of about fifty,
   * and paging a dashboard whose entire claim is "everything at a glance" would defeat it. When an
   * estate arrives that this is too small for, `next_page_token` is already in the response.
   */
  instances: () =>
    request<ListInstancesResponse>("/api/v1/instances?page_size=500"),
  backupAdherence: () =>
    request<GetBackupAdherenceResponse>("/api/v1/backup-adherence"),
};
