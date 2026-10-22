package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"pi-gateway/internal/egress"
	"pi-gateway/internal/oauth"
	"pi-gateway/internal/store"
)

// accountView is the API representation of an account (secrets omitted).
type accountView struct {
	*store.Account
	Inflight             int       `json:"inflight"`
	HasRefreshToken      bool      `json:"has_refresh_token"`
	HasChatGPTCredential bool      `json:"has_chatgpt_credential"`
	TokenExpiresInMS     int64     `json:"token_expires_in_ms"`
	ProxyDisplay         string    `json:"proxy_display"`
	Quota                quotaView `json:"quota"`
	// CodexLinked reports whether the optional quota credential is attached.
	CodexLinked bool `json:"codex_linked"`
}

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := s.store.ListAccounts(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]accountView, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, s.viewAccount(r.Context(), a))
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

// handleUnlinkCodex detaches the optional Codex quota credential from an account.
func (s *Server) handleUnlinkCodex(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	account, err := s.store.GetAccount(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if account == nil {
		writeErr(w, http.StatusNotFound, accountNotFoundMessage)
		return
	}
	if err := s.accounts.UnlinkCodexCredential(r.Context(), account); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.RecordAudit(r.Context(), "account.codex_unlinked", account.Name)
	updated, err := s.store.GetAccount(r.Context(), id)
	if err != nil || updated == nil {
		updated = account
	}
	writeJSON(w, http.StatusOK, map[string]any{"account": s.viewAccount(r.Context(), updated)})
}

func (s *Server) viewAccount(ctx context.Context, a *store.Account) accountView {
	var expiresIn int64
	if a.ExpiresAt > 0 {
		expiresIn = a.ExpiresAt - store.NowMS()
	}
	proxyDisplay := "direct"
	if strings.TrimSpace(a.ProxyURL) != "" {
		proxyDisplay = egress.Redact(a.ProxyURL)
	}
	public := *a
	public.ProxyURL = egress.Redact(a.ProxyURL)
	if public.GroupIDs == nil {
		public.GroupIDs = []int64{}
	}
	if public.DisabledModels == nil {
		public.DisabledModels = []string{}
	}
	if public.SupplementalModels == nil {
		public.SupplementalModels = []string{}
	}
	if public.InheritedModelRestrictions == nil {
		public.InheritedModelRestrictions = []store.InheritedModelRestriction{}
	}
	return accountView{
		Account:              &public,
		Inflight:             s.accounts.Inflight(a.ID),
		HasRefreshToken:      a.RefreshToken != "",
		HasChatGPTCredential: a.AccessToken != "",
		TokenExpiresInMS:     expiresIn,
		ProxyDisplay:         proxyDisplay,
		Quota:                s.buildQuotaView(ctx, a),
		CodexLinked:          a.CodexLinked(),
	}
}

// handleOAuthStart begins an OAuth login.
//
// kind=chatgpt (default) starts Sign in with ChatGPT — the credential used for
// model access. kind=codex starts the legacy Codex flow; its credential is only
// used to read quota. Either way the user lets the localhost:1455 callback finish
// it or pastes the resulting redirect URL back into the web UI.
//
// account_id binds the resulting credential to an existing account instead of
// creating a new one (used to attach the optional codex quota credential).
func (s *Server) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind      string          `json:"kind"`
		Name      string          `json:"name"`
		ProxyURL  *string         `json:"proxy_url"`
		ProxyID   json.RawMessage `json:"proxy_id"`
		AccountID string          `json:"account_id"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	selectedProxy, err := s.resolveProxySelection(r.Context(), body.ProxyID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	proxyURL := ""
	if body.ProxyURL != nil {
		proxyURL = strings.TrimSpace(*body.ProxyURL)
	}
	if selectedProxy != nil {
		proxyURL = selectedProxy.URL
	}
	// Absence means keep. An explicit empty URL or null proxy_id means direct.
	keepProxy := body.ProxyURL == nil && len(body.ProxyID) == 0
	accountRef := strings.TrimSpace(body.AccountID)
	var targetOK bool
	proxyURL, targetOK = s.oauthTargetProxy(w, r, accountRef, keepProxy, proxyURL)
	if !targetOK {
		return
	}
	if err := egress.Validate(proxyURL); err != nil {
		writeErr(w, http.StatusBadRequest, proxyURLPrefix+err.Error())
		return
	}

	kind := strings.ToLower(strings.TrimSpace(body.Kind))
	if kind == "" {
		kind = oauth.FlowKindChatGPT
	}
	if kind != oauth.FlowKindChatGPT && kind != oauth.FlowKindCodex {
		writeErr(w, http.StatusBadRequest, "kind must be chatgpt or codex")
		return
	}

	meta := map[string]string{
		"name":      strings.TrimSpace(body.Name),
		"proxy_url": proxyURL,
	}
	if len(body.ProxyID) > 0 {
		meta["proxy_id"] = string(body.ProxyID)
	} else if !keepProxy {
		meta["proxy_id"] = "null" // Explicit manual/direct choice replaces an old saved binding.
	}
	if id := strings.TrimSpace(body.AccountID); id != "" {
		meta["account_id"] = id
	}

	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()

	flow, err := s.flows.StartFlow(ctx, kind, meta)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}

	_ = s.store.RecordAudit(r.Context(), "account.oauth_started", flow.Kind)
	writeJSON(w, http.StatusOK, map[string]any{"flow": flow})
}

func (s *Server) handleOAuthStatus(w http.ResponseWriter, r *http.Request) {
	flowID := r.PathValue("flowID")
	flow := s.flows.Get(flowID)
	if flow == nil {
		writeErr(w, http.StatusNotFound, "login flow not found or expired")
		return
	}

	resp := map[string]any{"flow": flow}
	// Once authorized, surface the persisted account so the UI can show it.
	if acc := s.flowAccount(r.Context(), flow); acc != nil {
		resp["account"] = s.viewAccount(r.Context(), acc)
	}
	writeJSON(w, http.StatusOK, resp)
}

// flowAccount resolves the account a completed flow created or bound. It prefers
// the row id recorded by the completion callback, because the ChatGPT route's
// token carries no chatgpt-account-id to look up by.
func (s *Server) flowAccount(ctx context.Context, flow *oauth.Flow) *store.Account {
	if flow == nil || flow.Status != oauth.FlowCompleted {
		return nil
	}
	if flow.DBAccountID > 0 {
		if acc, err := s.store.GetAccount(ctx, flow.DBAccountID); err == nil && acc != nil {
			return acc
		}
	}
	if flow.AccountID != "" {
		if acc, err := s.store.FindAccountByAccountID(ctx, flow.AccountID); err == nil && acc != nil {
			return acc
		}
	}
	return nil
}

// handleOAuthComplete finishes a browser flow from a pasted redirect URL or code.
func (s *Server) handleOAuthComplete(w http.ResponseWriter, r *http.Request) {
	flowID := r.PathValue("flowID")
	var body struct {
		Input string `json:"input"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	flow, err := s.flows.SubmitInput(ctx, flowID, body.Input)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	resp := map[string]any{"flow": flow}
	if acc := s.flowAccount(ctx, flow); acc != nil {
		resp["account"] = s.viewAccount(r.Context(), acc)
		_ = s.store.RecordAudit(r.Context(), "account.oauth_completed", acc.Name)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleCreateAccount imports a ChatGPT credential only after verifying its refresh
// token and issued client id against the ChatGPT token endpoint. Supplied access
// tokens are screened for Codex claims, but are never persisted directly.
func (s *Server) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name           string          `json:"name"`
		CredentialType string          `json:"credential_type"`
		OAuthClientID  string          `json:"oauth_client_id"`
		RefreshToken   string          `json:"refresh_token"`
		AccessToken    string          `json:"access_token"`
		ProxyURL       string          `json:"proxy_url"`
		ProxyID        json.RawMessage `json:"proxy_id"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	selectedProxy, err := s.resolveProxySelection(r.Context(), body.ProxyID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if selectedProxy != nil {
		body.ProxyURL = selectedProxy.URL
	}
	if err := egress.Validate(body.ProxyURL); err != nil {
		writeErr(w, http.StatusBadRequest, proxyURLPrefix+err.Error())
		return
	}
	credentialType := strings.ToLower(strings.TrimSpace(body.CredentialType))
	if credentialType != "chatgpt" {
		writeErr(w, http.StatusBadRequest, "credential_type must be chatgpt; link Codex separately for quota and model catalog synchronization")
		return
	}
	if access := strings.TrimSpace(body.AccessToken); access != "" {
		if err := oauth.ValidateChatGPTAccessToken(access); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if strings.TrimSpace(body.RefreshToken) == "" {
		writeErr(w, http.StatusBadRequest, "a ChatGPT refresh_token is required to verify the credential source; use Sign in with ChatGPT for access-only credentials")
		return
	}
	clientID := strings.TrimSpace(body.OAuthClientID)
	if err := oauth.ValidateChatGPTClientID(clientID); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	proxyURL := strings.TrimSpace(body.ProxyURL)
	httpClient, err := s.factory.HTTPClient(proxyURL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	// Only a successful ChatGPT grant (including the direct-token scope) can
	// establish the source of an opaque refresh token. Never save the submitted
	// token first: a rejected grant must not create or overwrite an account.
	tok, err := oauth.NewClient(httpClient).RefreshChatGPT(ctx, strings.TrimSpace(body.RefreshToken), clientID)
	if err != nil {
		s.logger.Warn("ChatGPT credential import verification failed", "error", err)
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("initial ChatGPT refresh failed; account was not imported: %v", err))
		return
	}
	acc, err := s.accounts.CreateFromToken(ctx, strings.TrimSpace(body.Name), tok, proxyURL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(body.ProxyID) > 0 || proxyURL != "" {
		var selectedID int64
		if selectedProxy != nil {
			selectedID = selectedProxy.ID
		}
		if err := s.store.SetAccountProxy(ctx, acc.ID, selectedID, proxyURL); err != nil {
			writeErr(w, http.StatusInternalServerError, "could not persist account proxy")
			return
		}
		if fresh, err := s.store.GetAccount(ctx, acc.ID); err == nil && fresh != nil {
			acc = fresh
		}
	}
	_ = s.store.RecordAudit(r.Context(), "account.created", acc.Name)
	writeJSON(w, http.StatusOK, map[string]any{"account": s.viewAccount(r.Context(), acc)})
}

func (s *Server) handleUpdateAccount(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	acc, err := s.store.GetAccount(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if acc == nil {
		writeErr(w, http.StatusNotFound, accountNotFoundMessage)
		return
	}

	var body accountManagementMutation
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	if !applyAccountIdentity(w, acc, &body) {
		return
	}
	proxyChange, proxyOK := s.applyAccountProxy(w, r, acc, &body)
	if !proxyOK {
		return
	}
	if !applyAccountScheduling(w, acc, &body) {
		return
	}

	patch := store.AccountManagementPatch{
		Name: body.Name, Enabled: body.Enabled, Weight: body.Weight, Concurrency: body.Concurrency, Cooldown429Seconds: body.Cooldown429Seconds,
		GroupIDs: body.GroupIDs, DisabledModels: body.DisabledModels, SupplementalModels: body.SupplementalModels,
	}
	if body.UpstreamProtocol != nil {
		patch.UpstreamProtocol = &acc.UpstreamProtocol
	}
	if len(proxyChange) > 0 {
		patch.Proxy = &store.AccountProxyPatch{ID: acc.ProxyID, URL: acc.ProxyURL}
	}
	if err := s.store.PatchAccountManagementFields(r.Context(), acc.ID, patch); err != nil {
		writePolicyError(w, err)
		return
	}
	fresh, err := s.store.GetAccount(r.Context(), acc.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not reload account")
		return
	}
	if fresh == nil {
		writeErr(w, http.StatusNotFound, accountNotFoundMessage)
		return
	}
	acc = fresh
	_ = s.store.RecordAudit(r.Context(), "account.updated", acc.Name)
	writeJSON(w, http.StatusOK, map[string]any{"account": s.viewAccount(r.Context(), acc)})
}

func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	acc, _ := s.store.GetAccount(r.Context(), id)
	if err := s.store.DeleteAccount(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	name := fmt.Sprintf("#%d", id)
	if acc != nil {
		name = acc.Name
	}
	_ = s.store.RecordAudit(r.Context(), "account.deleted", name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleRecoverAccount(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	acc, err := s.store.GetAccount(r.Context(), id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if acc == nil {
		writeErr(w, 404, accountNotFoundMessage)
		return
	}
	if err := s.store.RecoverAccount(r.Context(), id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	updated, err := s.store.GetAccount(r.Context(), id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if updated == nil {
		writeErr(w, 404, accountNotFoundMessage)
		return
	}
	_ = s.store.RecordAudit(r.Context(), "account.recovered", acc.Name)
	writeJSON(w, 200, map[string]any{"account": s.viewAccount(r.Context(), updated)})
}

func (s *Server) handleRefreshAccount(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	acc, err := s.store.GetAccount(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if acc == nil {
		writeErr(w, http.StatusNotFound, accountNotFoundMessage)
		return
	}
	if acc.RefreshToken == "" {
		writeErr(w, http.StatusBadRequest, "account has no refresh token")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := s.accounts.Refresh(ctx, acc); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	updated, _ := s.store.GetAccount(r.Context(), id)
	_ = s.store.RecordAudit(r.Context(), "account.refreshed", acc.Name)
	writeJSON(w, http.StatusOK, map[string]any{"account": s.viewAccount(r.Context(), updated)})
}

// handleTestProxy verifies that an account's egress proxy can reach the upstream.
func (s *Server) handleTestProxy(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	acc, err := s.store.GetAccount(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if acc == nil {
		writeErr(w, http.StatusNotFound, accountNotFoundMessage)
		return
	}

	// Allow testing an unsaved value while editing.
	var body struct {
		ProxyURL *string `json:"proxy_url"`
	}
	_ = decodeJSON(r, &body)
	proxyURL := acc.ProxyURL
	if body.ProxyURL != nil {
		proxyURL = strings.TrimSpace(*body.ProxyURL)
	}

	// Account and saved-proxy tests share the same credential-free probe.
	result := s.probeProxy(r.Context(), proxyURL)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         result.Success,
		"status":     result.Status,
		"error":      result.Error,
		"latency_ms": result.LatencyMS,
		"proxy":      egress.Redact(proxyURL),
	})
}

func (s *Server) oauthTargetProxy(w http.ResponseWriter, r *http.Request, accountRef string, keepProxy bool, proxyURL string) (string, bool) {
	if accountRef != "" {
		id, parseErr := strconv.ParseInt(accountRef, 10, 64)
		if parseErr != nil || id <= 0 {
			writeErr(w, 400, "account_id must be a positive integer")
			return "", false
		}
		account, loadErr := s.store.GetAccount(r.Context(), id)
		if loadErr != nil {
			writeErr(w, 500, "could not load OAuth target account")
			return "", false
		}
		if account == nil {
			writeErr(w, 404, "OAuth target account not found")
			return "", false
		}
		if keepProxy {
			proxyURL = account.ProxyURL
		}
	}
	return proxyURL, true
}

type accountManagementMutation struct {
	Name               *string         `json:"name"`
	Enabled            *bool           `json:"enabled"`
	ProxyURL           *string         `json:"proxy_url"`
	ProxyID            json.RawMessage `json:"proxy_id"`
	Cooldown429Seconds *int            `json:"cooldown_429_seconds"`
	Concurrency        *int            `json:"concurrency"`
	Weight             *int            `json:"weight"`
	UpstreamProtocol   *string         `json:"upstream_protocol"`
	GroupIDs           []int64         `json:"group_ids"`
	DisabledModels     []string        `json:"disabled_models"`
	SupplementalModels []string        `json:"supplemental_models"`
}

func applyAccountIdentity(w http.ResponseWriter, acc *store.Account, body *accountManagementMutation) bool {
	if body.Name != nil {
		acc.Name = strings.TrimSpace(*body.Name)
	}
	if body.Enabled != nil {
		if *body.Enabled && acc.AccessToken == "" {
			writeErr(w, http.StatusBadRequest, "link ChatGPT before enabling this account")
			return false
		}
		acc.Enabled = *body.Enabled
	}

	return true
}

func (s *Server) applyAccountProxy(w http.ResponseWriter, r *http.Request, acc *store.Account, body *accountManagementMutation) ([]*int64, bool) {
	var proxyChange []*int64
	if len(body.ProxyID) > 0 {
		selected, err := s.resolveProxySelection(r.Context(), body.ProxyID)
		if err != nil {
			writeErr(w, 400, err.Error())
			return nil, false
		}
		acc.ProxyID = nil
		acc.ProxyURL = ""
		if selected != nil {
			acc.ProxyID = &selected.ID
			acc.ProxyURL = selected.URL
		}
		proxyChange = append(proxyChange, acc.ProxyID)
	}
	if body.ProxyURL != nil && (len(body.ProxyID) == 0 || acc.ProxyID == nil) {
		endpoint := strings.TrimSpace(*body.ProxyURL)
		// A redacted value from a legacy editor means unchanged, never a secret.
		if endpoint != egress.Redact(acc.ProxyURL) {
			if err := egress.Validate(endpoint); err != nil {
				writeErr(w, 400, proxyURLPrefix+err.Error())
				return nil, false
			}
			acc.ProxyURL = endpoint
			acc.ProxyID = nil
			proxyChange = []*int64{nil}
		}
	}

	return proxyChange, true
}

func applyAccountScheduling(w http.ResponseWriter, acc *store.Account, body *accountManagementMutation) bool {
	if body.Concurrency != nil {
		if *body.Concurrency < 1 {
			writeErr(w, http.StatusBadRequest, "concurrency must be at least 1")
			return false
		}
		acc.Concurrency = *body.Concurrency
	}
	if body.Weight != nil {
		if *body.Weight < 1 {
			writeErr(w, http.StatusBadRequest, "weight must be at least 1")
			return false
		}
		acc.Weight = *body.Weight
	}
	if body.UpstreamProtocol != nil {
		protocol := strings.ToLower(strings.TrimSpace(*body.UpstreamProtocol))
		// Empty clears the override and falls back to the global default.
		if protocol != "" && protocol != "sse" && protocol != "ws" {
			writeErr(w, http.StatusBadRequest, "upstream_protocol must be sse, ws or empty")
			return false
		}
		acc.UpstreamProtocol = protocol
	}
	if acc.Name == "" {
		writeErr(w, http.StatusBadRequest, "name cannot be empty")
		return false
	}

	return true
}
