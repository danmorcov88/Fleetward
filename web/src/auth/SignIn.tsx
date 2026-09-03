import { useState, type FormEvent } from "react";

import { api, ApiError } from "@/lib/api";

/**
 * The sign-in screen, and what it deliberately is not.
 *
 * It is **not a login form**. There is no username and no password, because Fleetward stores no
 * passwords and never will: authentication belongs to an identity provider (ADR-0008), and B10 is
 * where that arrives. What this asks for is an API token — the same credential the CLI uses — which
 * it posts to `POST /api/v1/sessions` in exchange for an HttpOnly cookie.
 *
 * The token is held in React state for exactly as long as the request takes, and then it is gone.
 * It is never put in localStorage or sessionStorage, because anything a script on this page can
 * read is something an injected script can steal, and a Fleetward token can restore a production
 * database.
 *
 * The screen says all of that in plain words rather than pretending to be a finished sign-in.
 * B4's rule was that a UI implying a protection it does not have is worse than one that says
 * nothing; the converse applies now that the protection is real, and being honest about what kind
 * of credential this is remains the same principle.
 */
export function SignIn({ onSignedIn }: { onSignedIn: () => void }) {
  const [token, setToken] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function submit(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await api.createSession(token.trim());
      // Dropped before anything else happens, so a re-render cannot put it back on the screen.
      setToken("");
      onSignedIn();
    } catch (err) {
      setError(
        err instanceof ApiError && err.status === 401
          ? "That token is not valid. It may have been revoked, or it may have expired."
          : err instanceof Error
            ? err.message
            : "The token could not be exchanged.",
      );
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="min-h-screen flex items-center justify-center p-8">
      <div className="w-full max-w-md">
        <div className="font-semibold tracking-tight text-lg">Fleetward</div>
        <p className="text-sm text-(--color-content-muted) mt-1">Operations control plane</p>

        <form onSubmit={submit} className="mt-8">
          <label htmlFor="token" className="block text-sm font-medium">
            API token
          </label>
          <p className="text-sm text-(--color-content-muted) mt-1">
            An administrator issues one with{" "}
            <code className="font-medium text-(--color-content)">
              fleetward token create --email you@example.com --role viewer
            </code>
            .
          </p>
          <input
            id="token"
            type="password"
            autoComplete="off"
            spellCheck={false}
            value={token}
            onChange={(e) => setToken(e.target.value)}
            placeholder="fwt_…"
            className="mt-3 w-full rounded-md border border-(--color-border-subtle) bg-(--color-surface) px-3 py-2 text-sm font-mono"
          />

          {error && (
            <div
              role="alert"
              className="mt-3 rounded-md border border-(--color-status-critical) p-3 text-sm"
            >
              {error}
            </div>
          )}

          <button
            type="submit"
            disabled={busy || token.trim() === ""}
            className="mt-4 w-full rounded-md border border-(--color-border-subtle) bg-(--color-surface-muted) px-3 py-2 text-sm font-medium disabled:opacity-50"
          >
            {busy ? "Exchanging…" : "Continue"}
          </button>
        </form>

        <p className="mt-8 text-xs text-(--color-content-muted) leading-relaxed">
          This is an interim credential. Fleetward stores no passwords, and sign-in through your own
          identity provider is the next step after this one. The token is exchanged for a session
          cookie the browser holds and this page cannot read; it is not saved anywhere in your
          browser.
        </p>
      </div>
    </div>
  );
}
