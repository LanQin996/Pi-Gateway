// Package oauth implements the two OpenAI subscription OAuth flows Pi uses:
//
//   - Sign in with ChatGPT (openai-chatgpt, see chatgpt.go) — the modern route
//     whose token is sent straight to api.openai.com. It powers model access.
//   - OpenAI Codex (legacy, this file) — the ChatGPT Plus/Pro flow that targets
//     the Codex backend. Its credential reads quota and the Codex model catalog.
//
// Both mirror Pi's parameters so the issued tokens are indistinguishable from Pi's.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Constants mirrored from Pi's auth/oauth/openai-codex.ts.
const (
	ClientID     = "app_EMoamEEZ73f0CkXaXp7hrann"
	AuthBaseURL  = "https://auth.openai.com"
	AuthorizeURL = AuthBaseURL + "/oauth/authorize"
	TokenURL     = AuthBaseURL + "/oauth/token"
	RedirectURI  = "http://localhost:1455/auth/callback"

	Scope = "openid profile email offline_access"

	// JWTClaimPath is the namespaced claim holding the ChatGPT account metadata.
	JWTClaimPath = "https://api.openai.com/auth"

	// DefaultOriginator is what Pi sends when creating the authorization flow.
	DefaultOriginator = "pi"
)

// Token is an OAuth token triple.
type Token struct {
	Access    string
	Refresh   string
	IDToken   string
	ExpiresAt time.Time
	AccountID string
	PlanType  string
	Email     string
	// ClientID is the client id OpenAI issued for a "Sign in with ChatGPT"
	// (dynamic_agent_client) registration. Refreshing that credential requires it.
	ClientID string
	// Scopes are the scopes the grant returned.
	Scopes []string
}

// PKCE holds a code verifier and its S256 challenge.
type PKCE struct {
	Verifier  string
	Challenge string
}

// GeneratePKCE mirrors Pi's generatePKCE: 32 random bytes, base64url (no padding),
// with an S256 challenge.
func GeneratePKCE() (PKCE, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return PKCE{}, fmt.Errorf("oauth: generate verifier: %w", err)
	}
	verifier := base64URLEncode(buf)
	sum := sha256.Sum256([]byte(verifier))
	return PKCE{Verifier: verifier, Challenge: base64URLEncode(sum[:])}, nil
}

func base64URLEncode(b []byte) string {
	return strings.TrimRight(base64.URLEncoding.EncodeToString(b), "=")
}

// GenerateState returns a 16-byte hex state value (Pi's createState).
func GenerateState() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("oauth: generate state: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// BuildAuthorizeURL builds the authorization URL with exactly the query parameters
// Pi sets, including originator=pi.
func BuildAuthorizeURL(challenge, state, originator string) string {
	if originator == "" {
		originator = DefaultOriginator
	}
	u, err := url.Parse(AuthorizeURL)
	if err != nil {
		return AuthorizeURL
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", ClientID)
	q.Set("redirect_uri", RedirectURI)
	q.Set("scope", Scope)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	q.Set("id_token_add_organizations", "true")
	q.Set("codex_cli_simplified_flow", "true")
	q.Set("originator", originator)
	u.RawQuery = q.Encode()
	return u.String()
}

type rawTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    *int   `json:"expires_in"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// Client performs the OAuth token operations and supports a custom HTTP client so
// a per-account proxy can be applied.
type Client struct {
	httpClient *http.Client
	originator string
	// chatgptTokenURL overrides ChatGPTTokenURL; it exists so tests can point the
	// Sign in with ChatGPT token endpoint at a stub server.
	chatgptTokenURL string
}

// NewClient builds an OAuth client. When httpClient is nil a default is used.
func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &Client{httpClient: httpClient, originator: DefaultOriginator, chatgptTokenURL: ChatGPTTokenURL}
}

// WithHTTPClient clones immutable OAuth configuration while binding a transport.
// It never mutates the shared client used by another concurrent login flow.
func (c *Client) WithHTTPClient(httpClient *http.Client) *Client {
	clone := *c
	clone.httpClient = httpClient
	return &clone
}

// SetOriginator overrides the originator used in the authorization URL.
func (c *Client) SetOriginator(v string) {
	if v != "" {
		c.originator = v
	}
}

// Originator reports the configured originator.
func (c *Client) Originator() string { return c.originator }

// StartBrowserFlow creates the PKCE material and the authorization URL.
func (c *Client) StartBrowserFlow() (pkce PKCE, state, authURL string, err error) {
	pkce, err = GeneratePKCE()
	if err != nil {
		return PKCE{}, "", "", err
	}
	state, err = GenerateState()
	if err != nil {
		return PKCE{}, "", "", err
	}
	return pkce, state, BuildAuthorizeURL(pkce.Challenge, state, c.originator), nil
}

// ExchangeCode swaps an authorization code for tokens.
func (c *Client) ExchangeCode(ctx context.Context, code, verifier, redirectURI string) (*Token, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {ClientID},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {redirectURI},
	}
	return c.postToken(ctx, form, "exchange")
}

// Refresh exchanges a refresh token for a new token triple.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (*Token, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {ClientID},
	}
	return c.postToken(ctx, form, "refresh")
}

func (c *Client) postToken(ctx context.Context, form url.Values, operation string) (*Token, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("oauth: %s request: %w", operation, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oauth: %s request failed: %w", operation, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("oauth: %s read body: %w", operation, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("oauth: token %s failed (%d): %s", operation, resp.StatusCode, safeErrorBody(body))
	}

	var raw rawTokenResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("oauth: %s decode: %w", operation, err)
	}
	if raw.AccessToken == "" {
		return nil, fmt.Errorf("oauth: %s response missing access_token: %s", operation, safeErrorBody(body))
	}
	if raw.ExpiresIn == nil {
		return nil, fmt.Errorf("oauth: %s response missing expires_in", operation)
	}

	tok := &Token{
		Access:    raw.AccessToken,
		Refresh:   raw.RefreshToken,
		IDToken:   raw.IDToken,
		ExpiresAt: time.Now().Add(time.Duration(*raw.ExpiresIn) * time.Second),
	}
	FillTokenMetadata(tok)
	return tok, nil
}

// safeErrorBody summarises a failed token response without echoing secrets back
// into error strings (and therefore logs): it prefers the OAuth error fields and
// otherwise reports only the size of the non-JSON body.
func safeErrorBody(body []byte) string {
	var payload struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &payload); err == nil {
		detail := strings.TrimSpace(payload.Error)
		if desc := strings.TrimSpace(payload.ErrorDescription); desc != "" {
			if detail != "" {
				detail += ": "
			}
			detail += desc
		}
		if detail != "" {
			return detail
		}
	}
	return fmt.Sprintf("<%d bytes of non-JSON error body>", len(body))
}

// FillTokenMetadata extracts account id, plan type and email from the JWT claims.
func FillTokenMetadata(tok *Token) {
	if tok == nil {
		return
	}
	if claims, err := DecodeJWT(tok.Access); err == nil {
		if auth, ok := claims[JWTClaimPath].(map[string]any); ok {
			if v, ok := auth["chatgpt_account_id"].(string); ok {
				tok.AccountID = v
			}
			if v, ok := auth["chatgpt_plan_type"].(string); ok {
				tok.PlanType = v
			}
		}
	}
	if tok.IDToken != "" {
		if claims, err := DecodeJWT(tok.IDToken); err == nil {
			if v, ok := claims["email"].(string); ok {
				tok.Email = v
			}
		}
	}
}

// DecodeJWT decodes a JWT payload without verifying the signature.
func DecodeJWT(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("oauth: not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Some emitters pad; try the padded encoding as a fallback.
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return nil, fmt.Errorf("oauth: decode JWT payload: %w", err)
		}
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("oauth: parse JWT payload: %w", err)
	}
	return claims, nil
}

// AccountIDFromToken extracts chatgpt_account_id from an access token.
func AccountIDFromToken(accessToken string) (string, error) {
	claims, err := DecodeJWT(accessToken)
	if err != nil {
		return "", fmt.Errorf("oauth: extract account id: %w", err)
	}
	auth, ok := claims[JWTClaimPath].(map[string]any)
	if !ok {
		return "", fmt.Errorf("oauth: token has no %s claim", JWTClaimPath)
	}
	id, _ := auth["chatgpt_account_id"].(string)
	if id == "" {
		return "", fmt.Errorf("oauth: token has no chatgpt_account_id")
	}
	return id, nil
}

// ValidateChatGPTAccessToken rejects recognizable legacy Codex token claims. This
// is a separation guard, not JWT signature verification. Opaque import credentials
// must be verified by refreshing against the ChatGPT token endpoint before use.
func ValidateChatGPTAccessToken(accessToken string) error {
	if strings.TrimSpace(accessToken) == "" {
		return errors.New("oauth: missing ChatGPT access token")
	}
	claims, err := DecodeJWT(accessToken)
	if err != nil {
		return nil // ChatGPT token responses may contain opaque access tokens.
	}
	if auth, ok := claims[JWTClaimPath].(map[string]any); ok {
		if _, present := auth["chatgpt_account_id"]; present {
			return errors.New("oauth: Codex access tokens cannot be used as ChatGPT credentials")
		}
	}
	for _, key := range []string{"client_id", "azp", "aud"} {
		if hasCodexClientID(claims[key]) {
			return errors.New("oauth: Codex client tokens cannot be used as ChatGPT credentials")
		}
	}
	return nil
}

// TokenExpiry extracts the exp claim from an access token.
func TokenExpiry(accessToken string) (time.Time, bool) {
	claims, err := DecodeJWT(accessToken)
	if err != nil {
		return time.Time{}, false
	}
	exp, ok := claims["exp"].(float64)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(int64(exp), 0), true
}

// ---- Authorization-code input helpers ----

// ParseAuthorizationInput accepts either a bare code, a full redirect URL, a
// "code=...&state=..." query, or a "code#state" pair. It mirrors Pi's
// parseAuthorizationInput so the web form behaves identically.
func ParseAuthorizationInput(input string) (code string, state string) {
	value := strings.TrimSpace(input)
	if value == "" {
		return "", ""
	}

	// 1. A full redirect URL: read the query string.
	if u, err := url.Parse(value); err == nil && u.Scheme != "" && u.Host != "" {
		q := u.Query()
		return q.Get("code"), q.Get("state")
	}

	// 2. "code#state" pair.
	if idx := strings.Index(value, "#"); idx >= 0 {
		return value[:idx], value[idx+1:]
	}

	// 3. Bare query string.
	if strings.Contains(value, "code=") {
		if parsed, err := url.ParseQuery(value); err == nil {
			return parsed.Get("code"), parsed.Get("state")
		}
	}

	// 4. A bare authorization code.
	return value, ""
}

func hasCodexClientID(value any) bool {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text) == ClientID
	}
	values, _ := value.([]any)
	for _, item := range values {
		if item == ClientID {
			return true
		}
	}
	return false
}
