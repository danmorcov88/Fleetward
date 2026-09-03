import type { ReactNode } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";

import { api, ApiError } from "@/lib/api";
import { SessionContext } from "./session";
import { SignIn } from "./SignIn";

/**
 * The gate in front of the whole application.
 *
 * It asks `GET /api/v1/me` once. That route is the only authenticated one that never fails on
 * authorization — any caller may ask who it is — so a 401 from it means exactly one thing, no
 * session, and nothing else has to be interpreted.
 *
 * Nothing here is a security control. The server refuses every request the caller may not make,
 * whatever this component renders; hiding a screen is a courtesy (ADR-0008). What this buys is that
 * a person without a session sees a sentence explaining how to get one, instead of an estate view
 * full of failed requests.
 */
export function AuthGate({ children }: { children: ReactNode }) {
  const queryClient = useQueryClient();

  const me = useQuery({
    queryKey: ["me"],
    queryFn: api.me,
    // A 401 is an answer, not a failure. Retrying it three times just delays the sign-in screen by
    // a couple of seconds and puts three more rows in the server's log.
    retry: (count, error) =>
      !(error instanceof ApiError && error.status === 401) && count < 2,
    staleTime: 60_000,
  });

  if (me.isPending) {
    return (
      <div className="min-h-screen flex items-center justify-center">
        <p className="text-sm text-(--color-content-muted)">Loading…</p>
      </div>
    );
  }

  if (me.error instanceof ApiError && me.error.status === 401) {
    return (
      <SignIn
        onSignedIn={() => {
          // Everything, not just the session: the estate queries that failed while there was no
          // session are cached as errors, and leaving them there would show a signed-in user a
          // screen full of the failures from before they signed in.
          void queryClient.invalidateQueries();
        }}
      />
    );
  }

  if (me.error || !me.data) {
    return (
      <div className="min-h-screen flex items-center justify-center p-8">
        <div className="max-w-md rounded-lg border border-(--color-status-critical) p-4">
          <p className="text-sm font-medium">The control plane could not be reached.</p>
          <p className="text-sm text-(--color-content-muted) mt-1">
            {(me.error as Error | undefined)?.message ?? "No answer from /api/v1/me."}
          </p>
        </div>
      </div>
    );
  }

  return <SessionContext.Provider value={me.data}>{children}</SessionContext.Provider>;
}
