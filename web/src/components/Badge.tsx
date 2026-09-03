import { cn } from "@/lib/cn";
import type { StatusTone } from "./StatusDot";

const TONE_CLASS: Record<StatusTone, string> = {
  ok: "border-(--color-status-ok) text-(--color-status-ok)",
  warn: "border-(--color-status-warn) text-(--color-status-warn)",
  critical:
    "border-(--color-status-critical) text-(--color-status-critical) bg-(--color-status-critical)/10 font-semibold",
  unknown: "border-(--color-border-subtle) text-(--color-content-muted)",
};

/**
 * A toned label for a state that has to be readable at a glance across fifty rows.
 *
 * `critical` is deliberately not just another colour: it is the only tone that fills its background
 * and thickens its text, so a failed verification is the loudest thing on the screen even in a
 * greyscale screenshot or to a reader who cannot distinguish red from green (CLAUDE.md §5).
 * `data-tone` carries the same information for a test, which must not be asserting about colours.
 */
export function Badge({
  tone,
  label,
  title,
  className,
}: {
  tone: StatusTone;
  label: string;
  title?: string;
  className?: string;
}) {
  return (
    <span
      data-tone={tone}
      title={title}
      className={cn(
        "inline-flex items-center rounded-md border px-1.5 py-0.5 text-xs whitespace-nowrap",
        TONE_CLASS[tone],
        className,
      )}
    >
      {label}
    </span>
  );
}
