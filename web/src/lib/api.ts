/**
 * Thin client for the control plane REST API.
 *
 * Stage 1 replaces the hand-written types here with types generated from the OpenAPI document the
 * control plane already emits, so that API drift becomes a compile error rather than a runtime
 * surprise.
 */

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

export interface VersionInfo {
  version: string;
  commit: string;
  date: string;
  go_version: string;
  platform: string;
  contract_version: string;
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
};
