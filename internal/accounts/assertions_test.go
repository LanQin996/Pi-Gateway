package accounts

import "testing"

// reportTestFailure keeps assertion diagnostics lazy: values are evaluated only
// when the original failure condition is met. It does not change Fatal/Error behavior.
func reportTestFailure(t testing.TB, failed bool, report func()) {
	t.Helper()
	if failed {
		report()
	}
}

// requireDataCondition retains the call site's location for concise contract assertions.
func requireDataCondition(t testing.TB, condition bool, message string) {
	t.Helper()
	if !condition {
		t.Fatal(message)
	}
}

// requireDataNoError reports failures at the operation's call site.
func requireDataNoError(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func requireDataTrue(t testing.TB, condition bool, args ...any) {
	t.Helper()
	if !condition {
		t.Fatal(args...)
	}
}
func requireDataTruef(t testing.TB, condition bool, format string, args ...any) {
	t.Helper()
	if !condition {
		t.Fatalf(format, args...)
	}
}
