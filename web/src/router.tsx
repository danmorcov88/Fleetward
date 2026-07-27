import {
  createRootRoute,
  createRoute,
  createRouter,
} from "@tanstack/react-router";

import { AppShell } from "./components/AppShell";
import { Estate } from "./routes/Estate";
import { System } from "./routes/System";

const rootRoute = createRootRoute({ component: AppShell });

const estateRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/",
  component: Estate,
});

const systemRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/system",
  component: System,
});

const routeTree = rootRoute.addChildren([estateRoute, systemRoute]);

export const router = createRouter({ routeTree });

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
