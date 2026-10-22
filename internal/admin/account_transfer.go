package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"pi-gateway/internal/oauth"
	"pi-gateway/internal/store"

	"github.com/google/uuid"
)

// Separate DTOs make credentials available only through the explicit backup API.
type portableCredential struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at"`
	AccountID    string `json:"account_id,omitempty"`
	ClientID     string `json:"client_id,omitempty"`
}
type portableAccount struct {
	Email   string              `json:"email"`
	ChatGPT *portableCredential `json:"chatgpt,omitempty"`
	Codex   *portableCredential `json:"codex,omitempty"`
}
type accountBundle struct {
	Type       string            `json:"type"`
	Version    int               `json:"version"`
	ExportedAt string            `json:"exported_at"`
	Accounts   []portableAccount `json:"accounts"`
}

func (a portableAccount) displayName() string {
	if a.Email != "" {
		return a.Email
	}
	if a.Codex != nil {
		return "codex-" + a.Codex.AccountID
	}
	return "ChatGPT"
}

func (a portableAccount) identity() string {
	if a.Email != "" {
		return "chatgpt:" + strings.ToLower(a.Email)
	}
	if a.Codex != nil {
		return "codex-import:" + a.Codex.AccountID
	}
	return "chatgpt:" + uuid.NewString()
}

func (a portableAccount) planType() string {
	c := a.ChatGPT
	if c == nil {
		c = a.Codex
	}
	tok := &oauth.Token{Access: c.AccessToken, IDToken: c.IDToken}
	oauth.FillTokenMetadata(tok)
	if tok.PlanType != "" {
		return tok.PlanType
	}
	if claims, err := oauth.DecodeJWT(c.IDToken); err == nil {
		if auth, ok := claims[oauth.JWTClaimPath].(map[string]any); ok {
			plan, _ := auth["chatgpt_plan_type"].(string)
			return plan
		}
	}
	return ""
}

func (s *Server) handleExportAccounts(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []int64 `json:"ids"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	ids, err := validAccountIDs(body.IDs)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	bundle := accountBundle{Type: gatewayAccountsFormat, Version: 1, ExportedAt: time.Now().UTC().Format(time.RFC3339), Accounts: []portableAccount{}}
	for _, id := range ids {
		a, err := s.store.GetAccount(r.Context(), id)
		if err != nil {
			writeErr(w, 500, "could not load account for export")
			return
		}
		if a == nil {
			writeErr(w, 404, "selected account no longer exists")
			return
		}
		out := portableAccount{Email: a.Email}
		if a.AccessToken != "" || a.RefreshToken != "" {
			out.ChatGPT = &portableCredential{AccessToken: a.AccessToken, RefreshToken: a.RefreshToken, IDToken: a.IDToken, ExpiresAt: a.ExpiresAt, ClientID: a.OAuthClientID}
		}
		if a.CodexAccessToken != "" || a.CodexRefreshToken != "" {
			out.Codex = &portableCredential{AccessToken: a.CodexAccessToken, RefreshToken: a.CodexRefreshToken, IDToken: a.CodexIDToken, ExpiresAt: a.CodexExpiresAt, AccountID: a.CodexAccountID}
		}
		bundle.Accounts = append(bundle.Accounts, out)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", `attachment; filename="pi-gateway-accounts.json"`)
	_ = s.store.RecordAudit(r.Context(), "account.exported", fmt.Sprintf("%d accounts", len(ids)))
	writeJSON(w, 200, bundle)
}

type accountImportRequest struct {
	Format          string          `json:"format"`
	Data            json.RawMessage `json:"data"`
	TargetAccountID int64           `json:"target_account_id,omitempty"`
}

func (s *Server) handleImportAccounts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var request accountImportRequest
	r.Body = http.MaxBytesReader(w, r.Body, 16<<20)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&request); err != nil {
		writeErr(w, 400, "invalid import JSON (maximum 16 MiB)")
		return
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		writeErr(w, 400, "import must contain one JSON document")
		return
	}
	records, format, err := decodeAccountImport(request.Format, request.Data)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if len(records) == 0 || len(records) > 1000 {
		writeErr(w, 400, "import must contain 1 to 1000 accounts")
		return
	}
	if request.TargetAccountID < 0 || (request.TargetAccountID > 0 && (format == gatewayFormat || len(records) != 1)) {
		writeErr(w, 400, "an explicit target requires one Codex credential")
		return
	}
	results := []accountOperationResult{}
	existing, err := s.store.ListAccounts(r.Context())
	if err != nil {
		writeErr(w, 500, "could not load existing accounts")
		return
	}
	for i, raw := range records {
		var result accountOperationResult
		result, existing = s.importAccountRecord(r.Context(), i, raw, format, request.TargetAccountID, existing)
		results = append(results, result)
	}
	_ = s.store.RecordAudit(r.Context(), "account.imported", fmt.Sprintf("%s: %d records", format, len(records)))
	writeJSON(w, 200, map[string]any{"format": format, "results": results})
}

func safeImportError(err error) string {
	// All parser/validation errors are deliberately field-only, never raw input.
	return err.Error()
}

func decodeAccountImport(format string, data json.RawMessage) ([]json.RawMessage, string, error) {
	var envelope accountImportEnvelope
	if len(data) == 0 {
		return nil, "", fmt.Errorf("select an account JSON file")
	}
	trimmedData := strings.TrimSpace(string(data))
	if !strings.HasPrefix(trimmedData, "{") && !strings.HasPrefix(trimmedData, "[") {
		return nil, "", fmt.Errorf("account data must be a JSON object or array")
	}
	if format == "" || format == "auto" {
		var err error
		format, err = detectAccountImportFormat(data)
		if err != nil {
			return nil, "", err
		}
	}
	if format != gatewayFormat && format != "sub2api" && format != "cliproxyapi" {
		return nil, "", fmt.Errorf("unsupported import format")
	}
	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, "[") {
		return decodeAccountArray(format, data)
	}
	if json.Unmarshal(data, &envelope) != nil {
		return nil, "", fmt.Errorf("invalid account document")
	}
	if format == gatewayFormat {
		return decodeGatewayEnvelope(envelope)
	}
	if format == "sub2api" {
		return decodeSub2APIEnvelope(envelope, data)
	}
	return []json.RawMessage{data}, format, nil
}

func decodeExternalCodex(format string, raw json.RawMessage) (portableAccount, error) {
	var src struct {
		Type         string                     `json:"type"`
		Platform     string                     `json:"platform"`
		Email        string                     `json:"email"`
		AccountID    string                     `json:"account_id"`
		AccessToken  string                     `json:"access_token"`
		RefreshToken string                     `json:"refresh_token"`
		IDToken      string                     `json:"id_token"`
		Expired      json.RawMessage            `json:"expired"`
		ExpiresAt    json.RawMessage            `json:"expires_at"`
		Credentials  map[string]json.RawMessage `json:"credentials"`
	}
	if json.Unmarshal(raw, &src) != nil {
		return portableAccount{}, fmt.Errorf("invalid credential field types")
	}
	c := &portableCredential{AccessToken: src.AccessToken, RefreshToken: src.RefreshToken, IDToken: src.IDToken, AccountID: src.AccountID}
	expiry := src.Expired
	if len(expiry) == 0 {
		expiry = src.ExpiresAt
	}
	if format == "sub2api" {
		if src.Platform != "openai" || src.Type != "oauth" {
			return portableAccount{}, fmt.Errorf("only sub2api openai/oauth Codex accounts are supported")
		}
		read := func(key string) string { var v string; _ = json.Unmarshal(src.Credentials[key], &v); return v }
		c.AccessToken = read("access_token")
		c.RefreshToken = read("refresh_token")
		c.IDToken = read("id_token")
		c.AccountID = read("chatgpt_account_id")
		if c.AccountID == "" {
			c.AccountID = read("account_id")
		}
		src.Email = read("email")
		expiry = src.Credentials["expires_at"]
	} else if src.Type != "codex" {
		return portableAccount{}, fmt.Errorf("CLIProxyAPI credential type must be codex")
	}
	c.ExpiresAt = parseCredentialExpiry(expiry)
	if len(expiry) > 0 && c.ExpiresAt == 0 && string(expiry) != "null" && string(expiry) != "0" && string(expiry) != `"0"` && string(expiry) != `""` {
		return portableAccount{}, fmt.Errorf("invalid credential expiry")
	}
	tok := &oauth.Token{Access: c.AccessToken, IDToken: c.IDToken}
	oauth.FillTokenMetadata(tok)
	if c.AccountID == "" {
		c.AccountID = tok.AccountID
	}
	if c.AccountID == "" {
		c.AccountID, _ = oauth.AccountIDFromToken(c.IDToken)
	}
	if src.Email == "" {
		src.Email = tok.Email
	}
	fillPortableExpiry(c)
	return portableAccount{Email: src.Email, Codex: c}, nil
}

func parseCredentialExpiry(raw json.RawMessage) int64 {
	var number int64
	if json.Unmarshal(raw, &number) == nil {
		if number > 0 && number < 100000000000 {
			return number * 1000
		}
		return number
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		if stamp, err := time.Parse(time.RFC3339Nano, value); err == nil {
			return stamp.UnixMilli()
		}
		if json.Unmarshal([]byte(value), &number) == nil {
			if number > 0 && number < 100000000000 {
				return number * 1000
			}
			return number
		}
	}
	return 0
}

func validatePortableAccount(a *portableAccount) error {
	a.Email = strings.TrimSpace(a.Email)
	if a.Email == "" {
		c := a.ChatGPT
		if c == nil {
			c = a.Codex
		}
		if c != nil {
			tok := &oauth.Token{Access: c.AccessToken, IDToken: c.IDToken}
			oauth.FillTokenMetadata(tok)
			a.Email = strings.TrimSpace(tok.Email)
		}
	}
	if len(a.Email) > 320 {
		return fmt.Errorf("email is too long")
	}
	if a.ChatGPT == nil && a.Codex == nil {
		return fmt.Errorf("ChatGPT or Codex credentials are required")
	}
	if err := validatePortableChatGPT(a.ChatGPT); err != nil {
		return err
	}
	if err := validatePortableCodex(a.Codex); err != nil {
		return err
	}

	return nil
}

func matchImportedAccount(accounts []*store.Account, entry portableAccount, target int64) (*store.Account, error) {
	var matches []*store.Account
	for _, a := range accounts {
		if target > 0 {
			if a.ID == target {
				matches = append(matches, a)
			}
			continue
		}
		if importedAccountMatches(a, entry) {
			matches = append(matches, a)
		}
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("multiple matching accounts; import one credential with an explicit target")
	}
	if len(matches) == 0 {
		if target > 0 {
			return nil, fmt.Errorf("target account not found")
		}
		return nil, nil
	}
	a := matches[0]
	if entry.Email != "" && a.Email != "" && !strings.EqualFold(entry.Email, a.Email) {
		return nil, fmt.Errorf("credential email does not match the target account")
	}
	return a, nil
}

func validatePortableChatGPT(credential *portableCredential) error {
	if credential != nil {
		c := credential
		if c.AccessToken == "" || c.RefreshToken == "" {
			return fmt.Errorf("ChatGPT access_token and refresh_token are required")
		}
		if err := oauth.ValidateChatGPTAccessToken(c.AccessToken); err != nil {
			return fmt.Errorf("ChatGPT access_token contains Codex claims")
		}
		if err := oauth.ValidateChatGPTClientID(c.ClientID); err != nil {
			return fmt.Errorf("invalid ChatGPT client_id")
		}
		if c.ExpiresAt < 0 || (c.ExpiresAt > 0 && c.ExpiresAt < 100000000000) {
			return fmt.Errorf("ChatGPT expires_at must use Unix milliseconds")
		}
	}
	return nil
}

func validatePortableCodex(credential *portableCredential) error {
	if credential != nil {
		c := credential
		if strings.TrimSpace(c.AccessToken) == "" || strings.TrimSpace(c.RefreshToken) == "" || strings.TrimSpace(c.AccountID) == "" {
			return fmt.Errorf("Codex access_token, refresh_token and account_id are required")
		}
		if tokenID, err := oauth.AccountIDFromToken(c.AccessToken); err == nil && tokenID != c.AccountID {
			return fmt.Errorf("Codex account_id conflicts with token claims")
		}
		if c.ExpiresAt < 0 || (c.ExpiresAt > 0 && c.ExpiresAt < 100000000000) {
			return fmt.Errorf("Codex expires_at must use Unix milliseconds")
		}
	}
	return nil
}

func importedAccountMatches(a *store.Account, entry portableAccount) bool {
	return (entry.Codex != nil && a.CodexAccountID == entry.Codex.AccountID) || (entry.Email != "" && strings.EqualFold(a.Email, entry.Email)) || (entry.Email == "" && entry.ChatGPT != nil && a.RefreshToken == entry.ChatGPT.RefreshToken)
}

func fillPortableExpiry(c *portableCredential) {
	if c.ExpiresAt == 0 {
		if expiry, ok := oauth.TokenExpiry(c.AccessToken); ok {
			c.ExpiresAt = expiry.UnixMilli()
		}
	}
}

type accountImportEnvelope struct {
	Type     string            `json:"type"`
	Version  int               `json:"version"`
	Accounts []json.RawMessage `json:"accounts"`
	Data     json.RawMessage   `json:"data"`
}

func detectAccountImportFormat(data json.RawMessage) (string, error) {
	var envelope accountImportEnvelope
	format := ""
	if strings.HasPrefix(strings.TrimSpace(string(data)), "[") {
		var items []json.RawMessage
		if json.Unmarshal(data, &items) != nil || len(items) == 0 {
			return "", fmt.Errorf("invalid or empty account array")
		}
		_, detected, err := decodeAccountImport("auto", items[0])
		if err != nil {
			return "", err
		}
		return detected, nil
	}
	_ = json.Unmarshal(data, &envelope)
	var probe struct {
		Platform    string          `json:"platform"`
		Credentials json.RawMessage `json:"credentials"`
	}
	_ = json.Unmarshal(data, &probe)
	switch {
	case envelope.Type == gatewayAccountsFormat:
		format = gatewayFormat
	case envelope.Type == sub2apiDataFormat || envelope.Type == sub2apiBundleFormat || len(envelope.Accounts) > 0 || len(envelope.Data) > 0 || (probe.Platform == "openai" && len(probe.Credentials) > 0):
		format = "sub2api"
	default:
		format = "cliproxyapi"
	}
	return format, nil
}

func decodeSub2APIEnvelope(envelope accountImportEnvelope, data json.RawMessage) ([]json.RawMessage, string, error) {
	format := "sub2api"
	if len(envelope.Data) > 0 {
		return decodeAccountImport("sub2api", envelope.Data)
	}
	if envelope.Type != "" && envelope.Type != sub2apiDataFormat && envelope.Type != sub2apiBundleFormat && envelope.Type != "oauth" {
		return nil, "", fmt.Errorf("unsupported sub2api bundle type")
	}
	if (envelope.Type == sub2apiDataFormat || envelope.Type == sub2apiBundleFormat) && envelope.Version != 1 {
		return nil, "", fmt.Errorf("unsupported sub2api bundle version")
	}
	if envelope.Accounts != nil {
		return envelope.Accounts, format, nil
	}
	return []json.RawMessage{data}, format, nil
}

func (s *Server) importAccountRecord(ctx context.Context, i int, raw json.RawMessage, format string, target int64, existing []*store.Account) (accountOperationResult, []*store.Account) {
	var err error
	result := accountOperationResult{Index: i, Status: "failed"}
	var entry portableAccount
	if format == gatewayFormat {
		err = json.Unmarshal(raw, &entry)
	} else {
		entry, err = decodeExternalCodex(format, raw)
	}
	if err == nil {
		err = validatePortableAccount(&entry)
	}
	if err != nil {
		result.Error = "invalid account: " + safeImportError(err)
		return result, existing
	}
	result.Name = entry.displayName()
	account, findErr := matchImportedAccount(existing, entry, target)
	if findErr != nil {
		result.Error = safeImportError(findErr)
		return result, existing
	}
	if format == gatewayFormat && account != nil {
		// Backup import is idempotent and never overwrites live rotating credentials.
		result.ID = account.ID
		result.Status = "skipped"
		return result, existing
	}
	if account != nil {
		s.attachImportedCodex(ctx, account, entry, &result)
	} else {
		a := importedStoreAccount(entry)
		if err = s.store.CreateAccount(ctx, a); err != nil {
			result.Error = "could not persist imported account"
		} else {
			result.ID = a.ID
			result.Status = "created"
			existing = append(existing, a)
		}
	}
	return result, existing
}

func (s *Server) attachImportedCodex(ctx context.Context, account *store.Account, entry portableAccount, result *accountOperationResult) {
	var err error
	if entry.Codex == nil {
		result.Error = "no Codex credential to attach"
	} else if account.CodexAccountID != "" && account.CodexAccountID != entry.Codex.AccountID {
		result.Error = "Codex identity does not match the existing account"
	} else {
		c := entry.Codex
		err = s.accounts.LinkCodexCredential(ctx, account, &oauth.Token{Access: c.AccessToken, Refresh: c.RefreshToken, IDToken: c.IDToken, AccountID: c.AccountID, ExpiresAt: time.UnixMilli(c.ExpiresAt)})
		if err == nil {
			result.Status = "linked"
			result.ID = account.ID
		} else {
			result.Error = "could not attach Codex credential"
		}
	}
}

func importedStoreAccount(entry portableAccount) *store.Account {
	a := &store.Account{Name: entry.displayName(), Email: entry.Email, AccountID: entry.identity(), PlanType: entry.planType(), Enabled: entry.ChatGPT != nil, Weight: 1, Concurrency: 3, Status: store.AccountStatusReady}

	if entry.ChatGPT != nil {
		c := entry.ChatGPT
		a.AccessToken = c.AccessToken
		a.RefreshToken = c.RefreshToken
		a.IDToken = c.IDToken
		a.ExpiresAt = c.ExpiresAt
		a.OAuthClientID = c.ClientID
	} else {
		a.Enabled = false
		a.Status = store.AccountStatusUnknown
	}
	if entry.Codex != nil {
		c := entry.Codex
		a.CodexAccessToken = c.AccessToken
		a.CodexRefreshToken = c.RefreshToken
		a.CodexIDToken = c.IDToken
		a.CodexExpiresAt = c.ExpiresAt
		a.CodexAccountID = c.AccountID
	}
	return a
}

func decodeAccountArray(format string, data json.RawMessage) ([]json.RawMessage, string, error) {
	if format == gatewayFormat {
		return nil, "", fmt.Errorf("pi-gateway import requires a versioned account bundle")
	}
	var records []json.RawMessage
	if json.Unmarshal(data, &records) != nil {
		return nil, "", fmt.Errorf("invalid account array")
	}
	return records, format, nil
}

func decodeGatewayEnvelope(envelope accountImportEnvelope) ([]json.RawMessage, string, error) {
	format := gatewayFormat
	if envelope.Type != gatewayAccountsFormat || envelope.Version != 1 {
		return nil, "", fmt.Errorf("unsupported pi-gateway account bundle type or version")
	}
	return envelope.Accounts, format, nil
}
