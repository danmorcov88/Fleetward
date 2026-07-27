// Package e2e holds end-to-end tests that drive the running stack through its public interfaces.
//
// The Stage 6 happy path lives here: bring the stack up, add an instance, run a backup, watch
// verification pass, and assert the two-part status the UI shows. It is deliberately separate from
// the conformance suite, which tests one plugin against the contract rather than the product
// against a user's workflow.
package e2e
