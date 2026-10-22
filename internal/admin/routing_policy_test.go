package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"pi-gateway/internal/accounts"
	"pi-gateway/internal/store"
)

func policyRequest(t *testing.T, handler http.HandlerFunc, method string, id int64, body string, want int) map[string]json.RawMessage {
	t.Helper()
	r := httptest.NewRequest(method, "/api/test", strings.NewReader(body))
	if id > 0 {
		r.SetPathValue("id", fmt.Sprint(id))
	}
	w := httptest.NewRecorder()
	handler(w, r)
	reportTestFailure(t, w.Code != want, func() {
		t.Fatalf("%s status=%d want=%d body=%s", method, w.Code, want, w.Body.String())
	})
	var out map[string]json.RawMessage
	{
		err := json.Unmarshal(w.Body.Bytes(), &out)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	return out
}

func TestAdminPolicyGroupAtomicCRUDAndEnvelope(t *testing.T) {
	st := newAdminTestStore(t)
	ctx := context.Background()
	s := &Server{store: st}
	a := &store.Account{Name: "fake account", Enabled: true}
	{
		err := st.CreateAccount(ctx, a)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	out := policyRequest(t, s.handleCreateAccountGroup, "POST", 0, `{"name":" New group "}`, 200)
	var group store.AccountGroup
	{
		err := json.Unmarshal(out["group"], &group)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	reportTestFailure(t, group.Name != "New group" || !group.Enabled || group.AccountIDs == nil || len(group.AccountIDs) != 0 || group.DisabledModels == nil, func() {
		t.Fatalf("new default group=%+v", group)
	})
	out = policyRequest(t, s.handleUpdateAccountGroup, "PATCH", group.ID, fmt.Sprintf(`{"name":" Renamed ","notes":" Notes ","enabled":false,"account_ids":[%d],"disabled_models":[" model-b ","model-a","model-b"]}`, a.ID), 200)
	{
		err := json.Unmarshal(out["group"], &group)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	reportTestFailure(t, group.Name != "Renamed" || group.Notes != "Notes" || group.Enabled || !reflect.DeepEqual(group.AccountIDs, []int64{a.ID}) || !reflect.DeepEqual(group.DisabledModels, []string{"model-a", "model-b"}), func() {
		t.Fatalf("full group patch=%+v", group)
	})
	policyRequest(t, s.handleUpdateAccountGroup, "PATCH", group.ID, `{"name":"must rollback","enabled":true,"account_ids":[999999],"disabled_models":[]}`, 400)
	saved, err := st.GetAccountGroup(ctx, group.ID)
	reportTestFailure(t, err != nil || saved.Name != group.Name || saved.Enabled || !reflect.DeepEqual(saved.DisabledModels, group.DisabledModels), func() {
		t.Fatalf("partial group HTTP mutation: %+v %v", saved, err)
	})
	out = policyRequest(t, s.handleUpdateAccountGroup, "PATCH", group.ID, `{"notes":"only notes"}`, 200)
	{
		err := json.Unmarshal(out["group"], &group)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	reportTestFailure(t, len(group.AccountIDs) != 1 || len(group.DisabledModels) != 2, func() {
		t.Fatalf("omitted group fields were cleared: %+v", group)
	})
	policyRequest(t, s.handleCreateAccountGroup, "POST", 0, `{"name":"renamed"}`, 409)
	policyRequest(t, s.handleCreateAccountGroup, "POST", 0, `{"name":"invalid members","account_ids":[999999]}`, 400)
	groups, err := st.ListAccountGroups(ctx)
	reportTestFailure(t, err != nil || len(groups) != 1, func() {
		t.Fatalf("failed create left partial row: %+v %v", groups, err)
	})
	out = policyRequest(t, s.handleSetAccountGroupAccounts, "PUT", group.ID, `{"account_ids":[]}`, 200)
	{
		err := json.Unmarshal(out["group"], &group)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	reportTestFailure(t, group.AccountIDs == nil || len(group.AccountIDs) != 0 || len(group.DisabledModels) != 2, func() {
		t.Fatalf("membership PUT envelope/lists=%+v", group)
	})
	out = policyRequest(t, s.handleUpdateAccountGroup, "PATCH", group.ID, `{"disabled_models":[]}`, 200)
	{
		err := json.Unmarshal(out["group"], &group)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	reportTestFailure(t, group.DisabledModels == nil || len(group.DisabledModels) != 0, func() {
		t.Fatalf("explicit model clear=%+v", group)
	})
	key := &store.APIKey{Name: "fake key", Enabled: true, GroupIDs: []int64{group.ID}}
	{
		err := st.CreateKey(ctx, key)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	policyRequest(t, s.handleDeleteAccountGroup, "DELETE", group.ID, "", 200)
	key, err = st.GetKey(ctx, key.ID)
	reportTestFailure(t, err != nil || len(key.GroupIDs) != 0, func() {
		t.Fatalf("delete left key binding: %+v %v", key, err)
	})
}

func TestAdminPolicyAccountPatchArraysAndInheritance(t *testing.T) {
	st := newAdminTestStore(t)
	ctx := context.Background()
	m := accounts.New(st, nil, nil, accounts.Options{})
	s := &Server{store: st, accounts: m}
	a := &store.Account{Name: "fake account", Enabled: true, AccessToken: "fake-hidden", RefreshToken: "fake-hidden-refresh"}
	{
		err := st.CreateAccount(ctx, a)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	g := &store.AccountGroup{Name: "Source group", Enabled: true, DisabledModels: []string{"inherited"}}
	{
		err := st.CreateAccountGroup(ctx, g)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	out := policyRequest(t, s.handleUpdateAccount, "PATCH", a.ID, fmt.Sprintf(`{"group_ids":[%d],"disabled_models":[" self ","self"]}`, g.ID), 200)
	var view store.Account
	{
		err := json.Unmarshal(out["account"], &view)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	reportTestFailure(t, !reflect.DeepEqual(view.GroupIDs, []int64{g.ID}) || !reflect.DeepEqual(view.DisabledModels, []string{"self"}) || !reflect.DeepEqual(view.InheritedModelRestrictions, []store.InheritedModelRestriction{{Model: "inherited", GroupNames: []string{"Source group"}}}), func() {
		t.Fatalf("account contract=%+v", view)
	})
	reportTestFailure(t, strings.Contains(string(out["account"]), "fake-hidden"), func() {
		t.Fatal("account view leaked token")
	})
	out = policyRequest(t, s.handleUpdateAccount, "PATCH", a.ID, `{"name":"renamed"}`, 200)
	{
		err := json.Unmarshal(out["account"], &view)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	reportTestFailure(t, len(view.GroupIDs) != 1 || len(view.DisabledModels) != 1, func() {
		t.Fatal("omitted policy was cleared")
	})
	policyRequest(t, s.handleUpdateAccount, "PATCH", a.ID, `{"name":"must rollback","group_ids":[99999],"disabled_models":[]}`, 400)
	saved, err := st.GetAccount(ctx, a.ID)
	reportTestFailure(t, err != nil || saved.Name != "renamed" || len(saved.GroupIDs) != 1 || len(saved.DisabledModels) != 1, func() {
		t.Fatalf("partial account patch: %+v %v", saved, err)
	})
	out = policyRequest(t, s.handleUpdateAccount, "PATCH", a.ID, `{"group_ids":[],"disabled_models":[]}`, 200)
	var raw map[string]json.RawMessage
	{
		err := json.Unmarshal(out["account"], &raw)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	for _, key := range []string{"group_ids", "disabled_models", "inherited_model_restrictions"} {
		reportTestFailure(t, string(raw[key]) != "[]", func() {
			t.Fatalf("%s must be [], got %s", key, raw[key])
		})
	}
	out = policyRequest(t, s.handleListAccounts, "GET", 0, "", 200)
	var listed []store.Account
	{
		err := json.Unmarshal(out["accounts"], &listed)
		reportTestFailure(t, err != nil || len(listed) != 1 || listed[0].GroupIDs == nil, func() {
			t.Fatalf("list contract=%+v %v", listed, err)
		})
	}
}

type policyInterleavedReader struct {
	once   sync.Once
	before func()
	reader io.Reader
}

func (r *policyInterleavedReader) Read(p []byte) (int, error) {
	r.once.Do(r.before)
	return r.reader.Read(p)
}

func TestAdminPolicyStaleSnapshotRenameDoesNotRestoreProxy(t *testing.T) {
	st := newAdminTestStore(t)
	ctx := context.Background()
	p := &store.Proxy{Name: "fake proxy", URL: "http://fake:secret@127.0.0.1:1"}
	{
		err := st.CreateProxy(ctx, p)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	a := &store.Account{Name: "before", Enabled: true, ProxyID: &p.ID, ProxyURL: p.URL}
	{
		err := st.CreateAccount(ctx, a)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	s := &Server{store: st, accounts: accounts.New(st, nil, nil, accounts.Options{})}
	// handleUpdateAccount loads its snapshot before decoding the body. This
	// reader performs a real unlink exactly between those two operations.
	body := &policyInterleavedReader{reader: strings.NewReader(`{"name":"after"}`), before: func() {
		{
			err := st.SetProxyAccounts(ctx, p.ID, []int64{})
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
		no := false
		{
			err := st.PatchAccountManagementFields(ctx, a.ID, store.AccountManagementPatch{Enabled: &no, DisabledModels: []string{"concurrent-ban"}})
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
	}}
	r := httptest.NewRequest("PATCH", "/api/accounts", body)
	r.SetPathValue("id", fmt.Sprint(a.ID))
	w := httptest.NewRecorder()
	s.handleUpdateAccount(w, r)
	reportTestFailure(t, w.Code != 200, func() {
		t.Fatalf("rename failed: %d %s", w.Code, w.Body.String())
	})
	got, err := st.GetAccount(ctx, a.ID)
	reportTestFailure(t, err != nil || got.Name != "after" || got.ProxyID != nil || got.ProxyURL != "" || got.Enabled || !reflect.DeepEqual(got.DisabledModels, []string{"concurrent-ban"}), func() {
		t.Fatalf("stale fields restored: %+v %v", got, err)
	})
}
