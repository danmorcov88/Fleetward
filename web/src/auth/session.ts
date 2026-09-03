import { createContext, useContext } from "react";

import type { GetMeResponse } from "@/lib/api";

/**
 * Who is signed in, for any component that needs to render it.
 *
 * Split from AuthGate so that file exports only a component — the react-refresh rule — but the
 * split earns its place anyway: this is the shape of the answer, and AuthGate is the thing that
 * decides whether there is one.
 *
 * Nothing read from here is a security control. The server refuses every request the caller may not
 * make, whatever a component chooses to render (ADR-0008); this exists so the screen can say who
 * you are, not so it can decide what you may do.
 */
export const SessionContext = createContext<GetMeResponse | null>(null);

/** useSession returns who is signed in. Inside AuthGate it is never null. */
export function useSession(): GetMeResponse {
  const session = useContext(SessionContext);
  if (!session) {
    throw new Error("useSession was called outside AuthGate");
  }
  return session;
}
