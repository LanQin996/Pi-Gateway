package admin

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"pi-gateway/internal/accounts"
	"pi-gateway/internal/compactprompt"
	"pi-gateway/internal/config"
	"pi-gateway/internal/settings"
	"pi-gateway/internal/store"
)

func TestCompactionSettingsValidationAndCatalog(t *testing.T) {
	st := newAdminTestStore(t)
	ctx := context.Background()
	cfg := config.Default()
	holder := settings.New(st, cfg)
	{
		err := holder.Load(ctx)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	s := &Server{cfg: cfg, store: st, settings: holder, accounts: accounts.New(st, nil, nil, accounts.Options{})}
	a := &store.Account{Name: "catalog", Enabled: true, SupplementalModels: []string{"custom-summary"}}
	{
		err := st.CreateAccount(ctx, a)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	out := policyRequest(t, s.handleGetSettings, "GET", 0, "", 200)
	var ids []string
	{
		err := json.Unmarshal(out["compaction_models"], &ids)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	reportTestFailure(t, !reflect.DeepEqual(ids, []string{"custom-summary", "gpt-6-luna"}), func() {
		t.Fatalf("suggestions=%v", ids)
	})
	reportTestFailure(t, holder.Get().CompactionMode != "on" || holder.Get().CompactionModel != "gpt-6-luna" || holder.Get().CompactionPrompt != compactprompt.Default, func() {
		t.Fatal("wrong defaults")
	})
	policyRequest(t, s.handlePutSettings, "PUT", 0, `{"compaction_mode":"auto","compaction_model":" custom-summary "}`, 200)
	reportTestFailure(t, holder.Get().CompactionMode != "auto" || holder.Get().CompactionModel != "custom-summary", func() {
		t.Fatal("settings not applied")
	})
	for _, body := range []string{`{"compaction_mode":"unknown"}`, `{"compaction_model":""}`, `{"compaction_model":"model with spaces"}`, `{"compaction_model":"line\nbreak"}`, `{"compaction_model":"` + strings.Repeat("x", 257) + `"}`} {
		policyRequest(t, s.handlePutSettings, "PUT", 0, body, 400)
		reportTestFailure(t, holder.Get().CompactionMode != "auto" || holder.Get().CompactionModel != "custom-summary", func() {
			t.Fatal("invalid request changed settings")
		})
	}
	policyRequest(t, s.handlePutSettings, "PUT", 0, `{"compaction_mode":"off"}`, 200)
	{
		err := holder.Load(ctx)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	reportTestFailure(t, holder.Get().CompactionMode != "off" || holder.Get().CompactionModel != "custom-summary", func() {
		t.Fatal("settings did not survive reload")
	})
	custom := "  保留任务进度。\n下一步：\n"
	body, _ := json.Marshal(map[string]any{"compaction_prompt": custom})
	policyRequest(t, s.handlePutSettings, "PUT", 0, string(body), 200)
	{
		err := holder.Load(ctx)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	reportTestFailure(t, holder.Get().CompactionPrompt != custom, func() {
		t.Fatal("custom prompt did not survive reload verbatim")
	})
	// Older clients omit the new field; updating other settings preserves it.
	policyRequest(t, s.handlePutSettings, "PUT", 0, `{"compaction_mode":"on"}`, 200)
	reportTestFailure(t, holder.Get().CompactionPrompt != custom, func() {
		t.Fatal("partial update lost custom prompt")
	})
	body, _ = json.Marshal(map[string]any{"compaction_prompt": strings.Repeat("a", compactprompt.MaxBytes+1)})
	policyRequest(t, s.handlePutSettings, "PUT", 0, string(body), 400)
	reportTestFailure(t, holder.Get().CompactionPrompt != custom, func() {
		t.Fatal("invalid prompt changed settings")
	})
	policyRequest(t, s.handlePutSettings, "PUT", 0, `{"compaction_prompt":" \n\t"}`, 200)
	{
		err := holder.Load(ctx)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	reportTestFailure(t, holder.Get().CompactionPrompt != compactprompt.Default, func() {
		t.Fatal("empty prompt did not restore Codex default")
	})
}
