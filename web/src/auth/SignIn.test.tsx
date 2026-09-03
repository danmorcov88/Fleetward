import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { SignIn } from "./SignIn";

/**
 * What is worth asserting about a sign-in screen, and what is not.
 *
 * Not: that it looks right. That is what a person looking at it is for.
 *
 * Worth it: that the credential goes to the server and stays nowhere else. A Fleetward token can
 * restore a production database, and the reason this screen exchanges one for an HttpOnly cookie is
 * that anything a script on this page can read is something an injected script can steal. A future
 * change that "helpfully" remembered the token would break nothing visible — which is exactly the
 * kind of regression a test has to catch.
 */
describe("SignIn", () => {
  beforeEach(() => {
    window.localStorage.clear();
    window.sessionStorage.clear();
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("exchanges the token and stores it nowhere in the browser", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify({ actor: "dan@example.com" }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );
    const onSignedIn = vi.fn();

    render(<SignIn onSignedIn={onSignedIn} />);
    await userEvent.type(screen.getByLabelText("API token"), "fwt_abc_def");
    await userEvent.click(screen.getByRole("button", { name: /continue/i }));

    expect(onSignedIn).toHaveBeenCalledOnce();

    const [path, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(path).toBe("/api/v1/sessions");
    expect(init.method).toBe("POST");
    expect(JSON.parse(init.body as string)).toEqual({ token: "fwt_abc_def" });

    // The whole point. Neither store may hold the credential, under any key.
    for (const store of [window.localStorage, window.sessionStorage]) {
      expect(store.length).toBe(0);
    }
    // And it is gone from the field, so a re-render cannot put it back on the screen.
    expect(screen.getByLabelText("API token")).toHaveValue("");
  });

  it("says a rejected token was rejected, without guessing why", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify({ title: "Unauthorized", status: 401 }), {
        status: 401,
        headers: { "Content-Type": "application/json" },
      }),
    );

    render(<SignIn onSignedIn={vi.fn()} />);
    await userEvent.type(screen.getByLabelText("API token"), "fwt_wrong_wrong");
    await userEvent.click(screen.getByRole("button", { name: /continue/i }));

    expect(await screen.findByRole("alert")).toHaveTextContent(/not valid/i);
  });
});
