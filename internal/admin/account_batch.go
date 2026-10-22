package admin

import (
	"context"
	"fmt"
	"net/http"
	"pi-gateway/internal/store"
)

type accountOperationResult struct {
	Index  int    `json:"index"`
	ID     int64  `json:"id,omitempty"`
	Name   string `json:"name,omitempty"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

func validAccountIDs(ids []int64) ([]int64, error) {
	if len(ids) == 0 || len(ids) > 1000 {
		return nil, fmt.Errorf("select 1 to 1000 accounts")
	}
	seen := map[int64]bool{}
	out := []int64{}
	for _, id := range ids {
		if id <= 0 {
			return nil, fmt.Errorf("account IDs must be positive integers")
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}

func (s *Server) handleBatchAccounts(w http.ResponseWriter, r *http.Request) {
	var body accountBatchRequest
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	ids, err := validAccountIDs(body.IDs)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if !s.validateBatchAction(w, r, body) {
		return
	}
	results := []accountOperationResult{}
	for i, id := range ids {
		result := s.applyBatchAccount(r.Context(), i, id, body)
		results = append(results, result)
	}
	_ = s.store.RecordAudit(r.Context(), "account.batch", fmt.Sprintf("%s: %d accounts", body.Action, len(ids)))
	writeJSON(w, 200, map[string]any{"results": results})
}

type accountBatchRequest struct {
	IDs      []int64 `json:"ids"`
	Action   string  `json:"action"`
	GroupIDs []int64 `json:"group_ids"`
}

func (s *Server) validateBatchAction(w http.ResponseWriter, r *http.Request, body accountBatchRequest) bool {
	switch body.Action {
	case "enable", "disable", "recover":
	case "add_groups":
		if len(body.GroupIDs) == 0 || len(body.GroupIDs) > 100 {
			writeErr(w, 400, "select 1 to 100 groups")
			return false
		}
		for _, id := range body.GroupIDs {
			if id <= 0 {
				writeErr(w, 400, "invalid group ID")
				return false
			}
			group, err := s.store.GetAccountGroup(r.Context(), id)
			if err != nil {
				writeErr(w, 500, "could not load account groups")
				return false
			}
			if group == nil {
				writeErr(w, 400, "account group not found")
				return false
			}
		}
	default:
		writeErr(w, 400, "action must be enable, disable, recover or add_groups")
		return false
	}
	return true
}

func (s *Server) applyBatchAccount(ctx context.Context, i int, id int64, body accountBatchRequest) accountOperationResult {
	result := accountOperationResult{Index: i, ID: id, Status: "success"}
	account, err := s.store.GetAccount(ctx, id)
	if err == nil && account == nil {
		err = fmt.Errorf("account not found")
	}
	if err == nil {
		result.Name = account.Name
		switch body.Action {
		case "enable", "disable":
			enabled := body.Action == "enable"
			if enabled && account.AccessToken == "" {
				err = fmt.Errorf("link ChatGPT before enabling this account")
			} else {
				err = s.store.PatchAccountManagementFields(ctx, id, store.AccountManagementPatch{Enabled: &enabled})
			}
		case "recover":
			err = s.store.RecoverAccount(ctx, id)
		case "add_groups":
			err = s.store.AddAccountGroups(ctx, id, body.GroupIDs)
		}
	}
	setBatchAccountError(&result, account, body.Action, err)
	return result
}

func setBatchAccountError(result *accountOperationResult, account *store.Account, action string, err error) {
	if err != nil {
		result.Status = "failed"
		result.Error = "account operation failed"
		if account == nil {
			result.Error = "account not found or unavailable"
		}
		if account != nil && action == "enable" && account.AccessToken == "" {
			result.Error = "link ChatGPT before enabling this account"
		}
	}
}
