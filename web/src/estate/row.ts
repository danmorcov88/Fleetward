import type { Instance, InstanceAdherence } from "@/lib/api";
import { severity } from "./status";

/**
 * EstateRow is one line of the estate view: what an instance is, how it is, and what its backups
 * are doing.
 *
 * It is assembled in the browser from two responses rather than served as one, and that is
 * deliberate. Health belongs to an instance and adherence belongs to a backup; putting either on
 * the other's endpoint would be a contract shaped by one screen. Both queries refetch on the same
 * interval, so the two halves of a row can never be more than one tick apart.
 */
export interface EstateRow extends InstanceAdherence {
  instance?: Instance;
}

/**
 * join merges the two responses and orders the result by severity.
 *
 * Adherence is the spine: it returns one entry per active instance, including the ones nobody has
 * declared anything for, which is exactly the set this screen must show. An instance is never
 * dropped for want of a matching inventory row — it would be a server disappearing from the estate
 * view because two responses disagreed for a tick, which is the failure mode a dashboard can least
 * afford.
 */
export function join(
  adherence: InstanceAdherence[] | undefined,
  instances: Instance[] | undefined,
): EstateRow[] {
  const byID = new Map((instances ?? []).map((i) => [i.id ?? "", i]));

  return (adherence ?? [])
    .map((a) => ({ ...a, instance: byID.get(a.instance_id ?? "") }))
    .sort((a, b) => {
      const bySeverity = severity(a) - severity(b);
      if (bySeverity !== 0) return bySeverity;
      return (a.instance_name ?? "").localeCompare(b.instance_name ?? "");
    });
}
