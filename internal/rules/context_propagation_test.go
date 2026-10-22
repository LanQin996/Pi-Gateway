package rules

import (
	"context"
	"errors"
	"testing"
)

// Exercise interpreter boundaries directly: Apply's initial cancellation check
// must not be the only point that observes a caller's cancellation.
func TestRuntimeBoundariesObserveExplicitContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	state := &evaluation{body: map[string]any{"unchanged": true}, vars: map[string]any{}}
	condition := &compiledCondition{raw: Condition{Op: "always"}}
	action := compiledAction{raw: Action{Type: "let", Params: map[string]any{"name": "changed", "value": true}}}
	checks := map[string]func() error{
		"computed expression": func() error {
			_, _, err := state.compute(ctx, map[string]any{"op": "array", "args": []any{}})
			return err
		},
		"nested expression": func() error { _, err := state.requiredExpression(ctx, expr("array", true)); return err },
		"condition":         func() error { _, err := condition.matches(ctx, state); return err },
		"action":            func() error { _, err := action.execute(ctx, state); return err },
		"flow steps":        func() error { _, err := state.runSteps(ctx, []compiledAction{action}); return err },
	}
	for name, check := range checks {
		t.Run(name, func(t *testing.T) {
			if err := check(); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation not propagated: %v", err)
			}
		})
	}
	assertJSON(t, state.body, map[string]any{"unchanged": true})
	if len(state.vars) != 0 {
		t.Fatal("canceled runtime mutated variables")
	}
}
