import { cn } from "@/lib/cn";

export type StatusTone = "ok" | "warn" | "critical" | "unknown";

const TONE_CLASS: Record<StatusTone, string> = {
  ok: "bg-(--color-status-ok)",
  warn: "bg-(--color-status-warn)",
  critical: "bg-(--color-status-critical)",
  unknown: "bg-(--color-status-unknown)",
};

/**
 * A status indicator that never relies on colour alone: the label beside it carries the same
 * information, which matters both for accessibility and for the screenshots operators paste into
 * incident channels.
 */
export function StatusDot({ tone, label }: { tone: StatusTone; label: string }) {
  return (
    <span className="inline-flex items-center gap-2 text-sm">
      <span className={cn("size-2 rounded-full shrink-0", TONE_CLASS[tone])} aria-hidden />
      <span>{label}</span>
    </span>
  );
}
