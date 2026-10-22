package codexbridge

import "testing"

// reportTestFailure keeps assertion diagnostics lazy: values are evaluated only
// when the original failure condition is met. It does not change Fatal/Error behavior.
func reportTestFailure(t testing.TB, failed bool, report func()) {
	t.Helper()
	if failed {
		report()
	}
}
