import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import type { Backup, InstanceAdherence } from "@/lib/api";
import { EstateTable } from "./EstateTable";
import { join } from "./row";

function backup(over: Partial<Backup> = {}): Backup {
  return {
    id: "b1",
    state: "BACKUP_STATE_SUCCEEDED",
    origin: "BACKUP_ORIGIN_MANAGED",
    completed_at: "2026-09-03T02:00:00Z",
    ...over,
  };
}

/** A fixture estate with one instance in each state the screen has to distinguish. */
const ESTATE: InstanceAdherence[] = [
  {
    instance_id: "1",
    instance_name: "adherent-and-verified",
    engine_type: "postgresql",
    state: "ADHERENCE_STATE_ADHERENT",
    satisfied_by: backup({ verification: { status: "VERIFICATION_STATUS_VERIFIED" } }),
  },
  {
    instance_id: "2",
    instance_name: "backed-up-and-proven-bad",
    engine_type: "sqlserver",
    state: "ADHERENCE_STATE_ADHERENT",
    satisfied_by: backup({ verification: { status: "VERIFICATION_STATUS_FAILED" } }),
  },
  {
    instance_id: "3",
    instance_name: "no-backup-at-all",
    engine_type: "postgresql",
    state: "ADHERENCE_STATE_MISSED",
  },
  {
    instance_id: "4",
    instance_name: "somebody-elses-backup",
    engine_type: "sqlserver",
    state: "ADHERENCE_STATE_ADHERENT",
    satisfied_by: backup({ origin: "BACKUP_ORIGIN_OBSERVED" }),
    // A source that does report an outcome, so the window is genuinely satisfied — but one that
    // assigns no identity, which is the caveat that survives.
    caveats: [
      "the directory assigns no identity of its own, so a backup that is moved or renamed appears as a new one",
    ],
  },
  {
    instance_id: "5",
    instance_name: "nobody-declared-anything",
    engine_type: "postgresql",
    state: "ADHERENCE_STATE_NOT_DECLARED",
  },
];

function renderEstate() {
  return render(<EstateTable rows={join(ESTATE, [])} />);
}

function rowFor(name: string): HTMLElement {
  const cell = screen.getByText(name);
  const tr = cell.closest("tr");
  if (!tr) throw new Error(`no row for ${name}`);
  return tr;
}

describe("EstateTable", () => {
  /**
   * CLAUDE.md §5 as an executable statement: a backup that succeeded and failed verification is
   * more dangerous than no backup at all, so it must be visually louder. The assertion is on the
   * tone rather than on a colour, because a test that asserts on a class name would pass a restyle
   * that made the critical state quieter.
   */
  it("renders a proven-bad backup louder than a missing one", () => {
    renderEstate();

    const provenBad = within(rowFor("backed-up-and-proven-bad")).getByText("verification failed");
    expect(provenBad).toHaveAttribute("data-tone", "critical");

    const missing = within(rowFor("no-backup-at-all")).getByText("—");
    expect(missing).not.toHaveAttribute("data-tone", "critical");
  });

  it("never offers a verification for a backup Fleetward did not take", () => {
    renderEstate();
    const observed = rowFor("somebody-elses-backup");
    expect(within(observed).getByText("n/a — not ours")).toBeInTheDocument();
    expect(within(observed).queryByText("never verified")).not.toBeInTheDocument();
    expect(within(observed).queryByText("verified")).not.toBeInTheDocument();
  });

  it("shows an instance nobody has declared anything for rather than hiding it", () => {
    renderEstate();
    expect(within(rowFor("nobody-declared-anything")).getByText("nothing declared"))
      .toBeInTheDocument();
  });

  it("opens on severity order, worst first", () => {
    renderEstate();
    const names = screen
      .getAllByTestId("estate-row")
      .map((tr) => tr.getAttribute("data-instance"));
    expect(names).toEqual([
      // Believed good and proven bad, then a window nothing satisfied, then the instance nobody
      // has declared anything for — a finding on an estate of fifty, and above the two that are
      // fine. An observed backup that satisfied its window is not a problem and does not rank as
      // one: it is as good as an answer gets for a backup Fleetward did not take.
      "backed-up-and-proven-bad",
      "no-backup-at-all",
      "nobody-declared-anything",
      "adherent-and-verified",
      "somebody-elses-backup",
    ]);
  });

  it("keeps a caveat behind the row, and reachable", async () => {
    renderEstate();
    const caveat =
      "the directory assigns no identity of its own, so a backup that is moved or renamed appears as a new one";
    expect(screen.queryByText(caveat)).not.toBeInTheDocument();

    await userEvent.click(screen.getByLabelText("Details for somebody-elses-backup"));
    expect(screen.getByText(caveat)).toBeInTheDocument();
  });
});
