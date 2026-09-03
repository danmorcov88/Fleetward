/** Formatting helpers shared by the estate view. Nothing here knows about the API. */

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

/**
 * age renders how long ago an instant was, in the coarsest unit that is still useful.
 *
 * The estate view uses it for every timestamp it shows, and that is the point rather than a
 * convenience: a health state or a backup time without its age is a claim, not a fact. "healthy" is
 * reassuring; "healthy · 3 weeks ago" is the same word telling the truth.
 */
export function age(iso: string | undefined, now: Date = new Date()): string {
  if (!iso) return "never";
  const then = new Date(iso);
  if (Number.isNaN(then.getTime())) return "never";

  const ms = now.getTime() - then.getTime();
  if (ms < 0) return "just now";
  if (ms < MINUTE) return "just now";
  if (ms < HOUR) return `${Math.floor(ms / MINUTE)}m ago`;
  if (ms < DAY) return `${Math.floor(ms / HOUR)}h ago`;
  return `${Math.floor(ms / DAY)}d ago`;
}

/** utc renders an instant the way the CLI does, so a screenshot and a terminal agree. */
export function utc(iso: string | undefined): string {
  if (!iso) return "—";
  const at = new Date(iso);
  if (Number.isNaN(at.getTime())) return "—";
  return at.toISOString().replace("T", " ").slice(0, 19) + " UTC";
}

/** bytes renders a size in the units a DBA reads backup sizes in. */
export function bytes(value: string | number | undefined): string {
  const n = typeof value === "string" ? Number(value) : value;
  if (n === undefined || n === null || Number.isNaN(n) || n <= 0) return "—";

  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let size = n;
  let unit = 0;
  while (size >= 1024 && unit < units.length - 1) {
    size /= 1024;
    unit += 1;
  }
  return `${size < 10 && unit > 0 ? size.toFixed(1) : Math.round(size)} ${units[unit]}`;
}

/** grace renders a grace period the way the schedule declared it. */
export function grace(minutes: number | undefined): string {
  if (!minutes) return "none";
  if (minutes % 60 === 0) return `${minutes / 60}h`;
  return `${minutes}m`;
}
