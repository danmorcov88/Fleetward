import { Link, Outlet, useRouterState } from "@tanstack/react-router";
import { useQueryClient } from "@tanstack/react-query";

import { useSession } from "@/auth/session";
import { api } from "@/lib/api";
import { cn } from "@/lib/cn";

/**
 * Navigation. Entries whose screens are not built yet are present but disabled, so the shape of the
 * product is visible from the first run rather than appearing a screen at a time.
 *
 * There is an account panel at the bottom, and until B6 there deliberately was not: every route was
 * open to anyone who could reach the port, and a UI that implied otherwise would have been claiming
 * a protection that did not exist. It now exists, so the screen says who is looking at it and what
 * they may do.
 *
 * The role shown is the strongest one this person holds *anywhere*. It is not permission to do
 * anything in particular — scope decides that, per request, on the server — and the panel says
 * "somewhere" rather than implying otherwise.
 */
const NAV = [
  { to: "/", label: "Estate", enabled: true },
  { to: "/backups", label: "Backups", enabled: false },
  { to: "/alerts", label: "Alerts", enabled: false },
  { to: "/admin", label: "Admin", enabled: false },
] as const;

export function AppShell() {
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  const session = useSession();
  const queryClient = useQueryClient();

  async function signOut() {
    await api.deleteSession();
    // The whole cache, because everything in it was read as somebody.
    queryClient.clear();
    await queryClient.invalidateQueries();
  }

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

        <div className="mt-auto p-3 flex flex-col gap-3">
          <div className="px-3 pt-3 border-t border-(--color-border-subtle)">
            <div className="text-sm truncate" title={session.caller?.actor}>
              {session.caller?.display_name || session.caller?.actor}
            </div>
            <div className="text-xs text-(--color-content-muted) mt-0.5">
              {session.highest_role ? `${session.highest_role} somewhere` : "no role"}
            </div>
            <button
              type="button"
              onClick={() => void signOut()}
              className="mt-2 text-xs text-(--color-content-muted) hover:text-(--color-content) underline"
            >
              Sign out
            </button>
          </div>

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
