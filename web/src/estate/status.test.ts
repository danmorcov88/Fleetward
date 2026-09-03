import { describe, expect, it } from "vitest";

import type { Backup, InstanceAdherence } from "@/lib/api";
import { severity, verificationCell } from "./status";
import { join } from "./row";

function backup(over: Partial<Backup> = {}): Backup {
  return {
    id: "b1",
    instance_id: "i1",
    state: "BACKUP_STATE_SUCCEEDED",
    origin: "BACKUP_ORIGIN_MANAGED",
    completed_at: "2026-09-03T02:00:00Z",
    ...over,
  };
}

/**
 * The one that matters.
 *
 * A cell that says "not verified" for a backup Fleetward did not take sends a DBA looking for a
 * verification that is never coming. "Not verified" and "cannot be verified" are different facts
 * and ADR-0015 exists to keep them apart, so every combination is enumerated rather than sampled.
 */
describe("verificationCell", () => {
  const cases: Array<{
    name: string;
    input: Backup | undefined;
    label: string;
    tone: string;
  }> = [
    { name: "no backup at all", input: undefined, label: "—", tone: "unknown" },
    {
      name: "observed, and no verification row",
      input: backup({ origin: "BACKUP_ORIGIN_OBSERVED" }),
      label: "n/a — not ours",
      tone: "unknown",
    },
    {
      name: "observed, even if something wrote a verification row",
      input: backup({
        origin: "BACKUP_ORIGIN_OBSERVED",
        verification: { status: "VERIFICATION_STATUS_VERIFIED" },
      }),
      label: "n/a — not ours",
      tone: "unknown",
    },
    {
      name: "managed, never verified",
      input: backup(),
      label: "never verified",
      tone: "warn",
    },
    {
      name: "managed and verified",
      input: backup({ verification: { status: "VERIFICATION_STATUS_VERIFIED" } }),
      label: "verified",
      tone: "ok",
    },
    {
      name: "managed and proven bad",
      input: backup({ verification: { status: "VERIFICATION_STATUS_FAILED" } }),
      label: "verification failed",
      tone: "critical",
    },
    {
      name: "managed, and the question could not be answered",
      input: backup({ verification: { status: "VERIFICATION_STATUS_INCONCLUSIVE" } }),
      label: "inconclusive",
      tone: "warn",
    },
  ];

  for (const c of cases) {
    it(`${c.name} reads "${c.label}"`, () => {
      const cell = verificationCell(c.input);
      expect(cell.label).toBe(c.label);
      expect(cell.tone).toBe(c.tone);
    });
  }

  it("never says 'never verified' about a backup Fleetward did not take", () => {
    for (const status of [
      undefined,
      "VERIFICATION_STATUS_VERIFIED",
      "VERIFICATION_STATUS_FAILED",
      "VERIFICATION_STATUS_INCONCLUSIVE",
    ] as const) {
      const cell = verificationCell(
        backup({
          origin: "BACKUP_ORIGIN_OBSERVED",
          verification: status ? { status } : undefined,
        }),
      );
      expect(cell.label).not.toContain("verified");
      expect(cell.label).not.toContain("failed");
    }
  });

  it("is the loudest tone only when a backup was believed good and proven bad", () => {
    const proven = verificationCell(
      backup({ verification: { status: "VERIFICATION_STATUS_FAILED" } }),
    );
    const missing = verificationCell(undefined);
    expect(proven.tone).toBe("critical");
    expect(missing.tone).not.toBe("critical");
  });
});

function row(over: Partial<InstanceAdherence> = {}): InstanceAdherence {
  return { instance_id: "i1", instance_name: "prod-1", ...over };
}

describe("severity", () => {
  it("ranks a proven-bad backup above a missing one", () => {
    const provenBad = row({
      state: "ADHERENCE_STATE_ADHERENT",
      satisfied_by: backup({ verification: { status: "VERIFICATION_STATUS_FAILED" } }),
    });
    const missed = row({ state: "ADHERENCE_STATE_MISSED" });
    expect(severity(provenBad)).toBeLessThan(severity(missed));
  });

  it("puts an instance nobody declared anything for above one that is fine", () => {
    const notDeclared = row({ state: "ADHERENCE_STATE_NOT_DECLARED" });
    const fine = row({
      state: "ADHERENCE_STATE_ADHERENT",
      satisfied_by: backup({ verification: { status: "VERIFICATION_STATUS_VERIFIED" } }),
    });
    expect(severity(notDeclared)).toBeLessThan(severity(fine));
  });

  it("does not treat an observed backup as an unverified one", () => {
    const observed = row({
      state: "ADHERENCE_STATE_ADHERENT",
      satisfied_by: backup({ origin: "BACKUP_ORIGIN_OBSERVED" }),
    });
    const unverified = row({ state: "ADHERENCE_STATE_ADHERENT", satisfied_by: backup() });
    expect(severity(observed)).toBeGreaterThan(severity(unverified));
  });
});

describe("join", () => {
  it("orders the estate worst-first, and breaks ties by name", () => {
    const rows = join(
      [
        row({ instance_id: "d", instance_name: "delta", state: "ADHERENCE_STATE_ADHERENT",
              satisfied_by: backup({ verification: { status: "VERIFICATION_STATUS_VERIFIED" } }) }),
        row({ instance_id: "b", instance_name: "bravo", state: "ADHERENCE_STATE_MISSED" }),
        row({ instance_id: "a", instance_name: "alpha", state: "ADHERENCE_STATE_ADHERENT",
              satisfied_by: backup({ verification: { status: "VERIFICATION_STATUS_FAILED" } }) }),
        row({ instance_id: "c", instance_name: "charlie", state: "ADHERENCE_STATE_MISSED" }),
      ],
      [],
    );
    expect(rows.map((r) => r.instance_name)).toEqual(["alpha", "bravo", "charlie", "delta"]);
  });

  it("keeps an instance the inventory response has not caught up with", () => {
    const rows = join([row({ instance_id: "i1", instance_name: "prod-1" })], []);
    expect(rows).toHaveLength(1);
    expect(rows[0].instance).toBeUndefined();
  });

  it("attaches health when both responses agree", () => {
    const rows = join(
      [row({ instance_id: "i1" })],
      [{ id: "i1", health: "HEALTH_STATE_DOWN", last_seen_at: "2026-08-01T00:00:00Z" }],
    );
    expect(rows[0].instance?.health).toBe("HEALTH_STATE_DOWN");
  });
});
