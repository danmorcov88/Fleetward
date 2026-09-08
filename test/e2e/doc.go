// Package e2e drives the whole product through its public interfaces, on a stack it brings up
// itself.
//
// It holds one test, and that test is the demo. `demo_test.go` calls the same acts `go run
// ./tools/demo` calls, with the narration's ceremony off and every assertion on, so the two cannot
// diverge without this package going red (ADR-0037).
//
// It is deliberately separate from the conformance suite, which tests one plugin against the
// contract rather than the product against a user's workflow, and from the integration suites,
// which test one service against a database. Neither of those exercises the thing somebody deciding
// whether to trust this actually wants to see: a backup taken, restored into a throwaway container,
// checked against the manifest captured when it was taken, and then the same check going red on an
// artifact whose bytes were deliberately changed.
//
// It is behind the `e2e` build tag because it needs Docker, several minutes, and sole use of the
// daemon. `make demo-check` runs it.
package e2e
