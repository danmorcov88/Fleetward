import { Link, Outlet, useRouterState } from "@tanstack/react-router";
import { cn } from "@/lib/cn";

/**
 * Navigation. Entries whose screens are not built yet are present but disabled, so the shape of the
 * product is visible from the first run rather than appearing a screen at a time.
 *
 * There is deliberately no account menu and no "signed in as". Every route under `/api/v1/` is open
 * to anyone who can reach the port, and a UI that implied otherwise would be claiming a protection
 * that does not exist. Authorization is a later slice, and until it lands this screen says nothing
 * about who is looking at it.
 */
const NAV = [
  { to: "/", label: "Estate", enabled: true },
  { to: "/backups", label: "Backups", enabled: false },
  { to: "/alerts", label: "Alerts", enabled: false },
  { to: "/admin", label: "Admin", enabled: false },
] as const;

export function AppShell() {
  const pathname = useRouterState({ select: (s) => s.location.pathname });

  return (
    <div className="min-h-screen flex">
      <aside className="w-56 shrink-0 border-r border-(--color-border-subtle) bg-(--color-surface-muted) flex flex-col">
        <div className="px-5 py-5 border-b border-(--color-border-subtle)">
          <div className="font-semibold tracking-tight">Fleetward</div>
          <div className="text-xs text-(--color-content-muted) mt-0.5">Operations control plane</div>
        </div>

        <nav className="p-3 flex flex-col gap-0.5">
          {NAV.map((item) =>
            item.enabled ? (
              <Link
                key={item.to}
                to={item.to}
                className={cn(
                  "px-3 py-2 rounded-md text-sm transition-colors",
                  pathname === item.to
                    ? "bg-(--color-surface) font-medium"
                    : "text-(--color-content-muted) hover:bg-(--color-surface)",
                )}
              >
                {item.label}
              </Link>
            ) : (
              <span
                key={item.to}
                className="px-3 py-2 rounded-md text-sm text-(--color-content-muted) opacity-50 cursor-not-allowed"
                title="Not built yet"
              >
                {item.label}
              </span>
            ),
          )}
        </nav>

        <div className="mt-auto p-3">
          <Link
            to="/system"
            className={cn(
              "block px-3 py-2 rounded-md text-sm transition-colors",
              pathname === "/system"
                ? "bg-(--color-surface) font-medium"
                : "text-(--color-content-muted) hover:bg-(--color-surface)",
            )}
          >
            System status
          </Link>
        </div>
      </aside>

      <main className="flex-1 min-w-0">
        <Outlet />
      </main>
    </div>
  );
}
