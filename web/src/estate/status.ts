/**
 * The estate view's whole vocabulary, in one file with no React in it.
 *
 * It is separate from the components because it is the part that can be *wrong* in a way nobody
 * notices: a cell that renders "not verified" for a backup Fleetward did not take is a sentence
 * that sends a DBA looking for a verification that is never coming. Keeping the mapping pure means
 * the test can enumerate every combination instead of asserting about pixels.
 *
 * The words are not new. `fleetward-cli backup history` has been saying them since B3, and the two
 * surfaces must not disagree about what a state is called.
 */

import type {
  AdherenceState,
  Backup,
  HealthState,
  InstanceAdherence,
} from "@/lib/api";
import type { StatusTone } from "@/components/StatusDot";

/** A cell's rendered answer: what it says, how loud it is, and the detail underneath it. */
export interface Cell {
  label: string;
  tone: StatusTone;
  detail?: string;
}

// -------------------------------------------------------------------------------------------
// Verification — the differentiator, and the one that must never collapse into two states
// -------------------------------------------------------------------------------------------

/**
 * verificationCell answers "is this backup known to be restorable" for one backup.
 *
 * Three facts, not two. A managed backup that has never been verified is a *gap*: verification is
 * possible and has not happened. An observed backup is a *permanent absence*: Fleetward did not
 * take it, so there is no manifest to compare a restore against, and there never will be
 * (ADR-0015). Rendering the second as the first is the specific failure this product exists to
 * prevent, so the observed case says whose backup it is rather than what is missing.
 *
 * And a failed verification is the loudest thing on this screen — louder than no backup at all,
 * because a backup believed good and proven bad is more dangerous than a known-missing one
 * (CLAUDE.md §5).
 */
export function verificationCell(backup: Backup | undefined): Cell {
  if (!backup) {
    return { label: "—", tone: "unknown" };
  }
  if (backup.origin === "BACKUP_ORIGIN_OBSERVED") {
    return {
      label: "n/a — not ours",
      tone: "unknown",
      detail: "Fleetward did not take this backup, so it carries no manifest to verify against",
    };
  }

  const status = backup.verification?.status;
  switch (status) {
    case "VERIFICATION_STATUS_VERIFIED":
      return { label: "verified", tone: "ok" };
    case "VERIFICATION_STATUS_FAILED":
      return {
        label: "verification failed",
        tone: "critical",
        detail:
          backup.verification?.error_message ||
          "a restore of this backup did not match the manifest taken with it",
      };
    case "VERIFICATION_STATUS_INCONCLUSIVE":
      return {
        label: "inconclusive",
        tone: "warn",
        detail:
          backup.verification?.error_message ||
          "the question could not be answered, which is not the same as a bad backup",
      };
    default:
      return { label: "never verified", tone: "warn" };
  }
}

// -------------------------------------------------------------------------------------------
// Adherence — did the backup happen when it was declared to
// -------------------------------------------------------------------------------------------

const ADHERENCE: Record<AdherenceState, Cell> = {
  ADHERENCE_STATE_ADHERENT: { label: "adherent", tone: "ok" },
  ADHERENCE_STATE_MISSED: { label: "missed", tone: "critical" },
  ADHERENCE_STATE_UNPROVEN: {
    label: "unproven",
    tone: "warn",
    detail: "something arrived inside the window and cannot say whether it worked",
  },
  ADHERENCE_STATE_FAILED: { label: "backup failed", tone: "critical" },
  ADHERENCE_STATE_NOT_DECLARED: {
    label: "nothing declared",
    tone: "unknown",
    detail: "nobody has said when this instance's backups are supposed to happen",
  },
  ADHERENCE_STATE_UNSPECIFIED: { label: "unknown", tone: "unknown" },
};

export function adherenceCell(state: AdherenceState | undefined): Cell {
  return ADHERENCE[state ?? "ADHERENCE_STATE_UNSPECIFIED"];
}

// -------------------------------------------------------------------------------------------
// Health
// -------------------------------------------------------------------------------------------

const HEALTH: Record<HealthState, Cell> = {
  HEALTH_STATE_UP: { label: "healthy", tone: "ok" },
  HEALTH_STATE_DEGRADED: { label: "degraded", tone: "warn" },
  HEALTH_STATE_DOWN: { label: "down", tone: "critical" },
  HEALTH_STATE_UNKNOWN: { label: "never probed", tone: "unknown" },
  HEALTH_STATE_UNSPECIFIED: { label: "never probed", tone: "unknown" },
};

export function healthCell(state: HealthState | undefined): Cell {
  return HEALTH[state ?? "HEALTH_STATE_UNSPECIFIED"];
}

// -------------------------------------------------------------------------------------------
// Ordering
// -------------------------------------------------------------------------------------------

/**
 * severity ranks a row for the default sort: the worst thing on the estate is the first thing read.
 *
 * A screen sorted by name makes the reader do the scanning the screen exists to do. The order below
 * is an argument rather than a preference, and it is the same argument CLAUDE.md §5 makes:
 *
 *   0  a backup that succeeded and was proven bad — believed good, and it is not
 *   1  a window that no backup satisfied, or one a backup failed in
 *   2  a window satisfied only by evidence that cannot report an outcome
 *   3  a managed backup nobody has verified — possible, and not done
 *   4  an instance nobody has declared anything for
 *   5  everything that is fine
 *
 * Health is deliberately not in this ranking. A server that is down right now and backed up
 * correctly last night is an operational problem; a server that is up and whose last backup is
 * unrestorable is a data-loss problem, and this screen is about the second. Health is sortable by
 * its own column for anyone who wants the other question.
 */
export function severity(row: InstanceAdherence): number {
  const backup = row.satisfied_by ?? row.latest_backup;
  const verification = verificationCell(backup);

  if (verification.label === "verification failed") return 0;
  if (row.state === "ADHERENCE_STATE_MISSED" || row.state === "ADHERENCE_STATE_FAILED") return 1;
  if (row.state === "ADHERENCE_STATE_UNPROVEN") return 2;
  if (verification.label === "never verified") return 3;
  if (row.state === "ADHERENCE_STATE_NOT_DECLARED") return 4;
  return 5;
}
