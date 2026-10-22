package admin

import (
	"context"
	"strings"
	"testing"

	"pi-gateway/internal/accounts"
	"pi-gateway/internal/store"
)

func TestManualAccountRecovery(t *testing.T) {
	st := newAdminTestStore(t)
	ctx := context.Background()
	s := &Server{store: st, accounts: accounts.New(st, nil, nil, accounts.Options{})}
	a := &store.Account{Name: "recover", Enabled: false, Status: store.AccountStatusBanned, AccessToken: "private-token", RefreshToken: "private-refresh", Weight: 2, Concurrency: 3}
	{
		err := st.CreateAccount(ctx, a)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	_, err := st.ExecContext(ctx, `UPDATE accounts SET last_error='denied',consecutive_failures=5,cooldown_until=9999999999999,cooldown_kind='rate_limit',ewma_failure_rate_bp=9000,request_count=20,error_count=3 WHERE id=?`, a.ID)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	out := policyRequest(t, s.handleRecoverAccount, "POST", a.ID, "", 200)
	reportTestFailure(t, strings.Contains(string(out["account"]), "private-"), func() {
		t.Fatal("credential exposed")
	})
	fresh, err := st.GetAccount(ctx, a.ID)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	reportTestFailure(t, fresh.Status != store.AccountStatusReady || fresh.LastError != "" || fresh.ConsecutiveFailures != 0 || fresh.CooldownUntil != 0 || fresh.CooldownKind != "" || fresh.EWMAFailureRateBP != 0, func() {
		t.Fatalf("health not restored: %+v", fresh)
	})
	reportTestFailure(t, fresh.Enabled || fresh.AccessToken != a.AccessToken || fresh.RefreshToken != a.RefreshToken || fresh.RequestCount != 20 || fresh.ErrorCount != 3 || fresh.Weight != 2 || fresh.Concurrency != 3, func() {
		t.Fatalf("unrelated state changed: %+v", fresh)
	})
	events, err := st.ListAudit(ctx, 10)
	reportTestFailure(t, err != nil || len(events) != 1 || events[0].Action != "account.recovered", func() {
		t.Fatalf("audit=%+v err=%v", events, err)
	})
	policyRequest(t, s.handleRecoverAccount, "POST", 999999, "", 404)
}
