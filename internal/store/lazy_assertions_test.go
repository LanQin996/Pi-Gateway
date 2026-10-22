package store

import "testing"

// reportStoreTestFailure keeps assertion diagnostics lazy: values are evaluated only
// when the original failure condition is met. It does not change Fatal/Error behavior.
func reportStoreTestFailure(t testing.TB, failed bool, report func()) {
	t.Helper()
	if failed {
		report()
	}
}
