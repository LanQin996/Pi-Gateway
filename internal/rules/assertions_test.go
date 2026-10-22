package rules

import "testing"

// requireCondition retains the call site's location for concise contract assertions.
func requireCondition(t testing.TB, condition bool, message string) {
	t.Helper()
	if !condition {
		t.Fatal(message)
	}
}

// requireNoError reports failures at the operation's call site.
func requireNoError(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func requireTrue(t testing.TB, condition bool, args ...any) {
	t.Helper()
	if !condition {
		t.Fatal(args...)
	}
}
func requireTruef(t testing.TB, condition bool, format string, args ...any) {
	t.Helper()
	if !condition {
		t.Fatalf(format, args...)
	}
}
