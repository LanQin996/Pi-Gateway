package accounts_test

import "testing"

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
