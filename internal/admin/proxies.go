package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"pi-gateway/internal/egress"
	"pi-gateway/internal/oauth"
	"pi-gateway/internal/store"
)

type proxyView struct {
	*store.Proxy
	URL    string `json:"url"`
	Scheme string `json:"scheme"`
}

func viewProxy(p *store.Proxy) proxyView {
	u, _ := egress.Parse(p.URL)
	scheme := ""
	if u != nil {
		scheme = u.Scheme
	}
	return proxyView{Proxy: p, URL: egress.Redact(p.URL), Scheme: scheme}
}

func proxyPagination(r *http.Request) (page, size int, err error) {
	page, size = 1, 20
	for key, target := range map[string]*int{"page": &page, "page_size": &size} {
		if value := r.URL.Query().Get(key); value != "" {
			*target, err = strconv.Atoi(value)
			if err != nil || *target < 1 {
				return 0, 0, fmt.Errorf("%s must be a positive integer", key)
			}
		}
	}
	if size > 100 || page > 1000000 {
		return 0, 0, errors.New("page_size must be at most 100 and page at most 1000000")
	}
	return
}

func (s *Server) handleListProxies(w http.ResponseWriter, r *http.Request) {
	page, size, err := proxyPagination(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	items, total, err := s.store.ListProxies(r.Context(), r.URL.Query().Get("search"), size, (page-1)*size)
	if err != nil {
		writeErr(w, 500, "could not load proxies")
		return
	}
	out := make([]proxyView, 0, len(items))
	for _, p := range items {
		out = append(out, viewProxy(p))
	}
	writeJSON(w, 200, map[string]any{"items": out, "total": total, "page": page, "page_size": size})
}

func (s *Server) loadProxy(w http.ResponseWriter, r *http.Request) *store.Proxy {
	id, err := pathInt(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return nil
	}
	p, err := s.store.GetProxy(r.Context(), id)
	if err != nil {
		writeErr(w, 500, "could not load proxy")
		return nil
	}
	if p == nil {
		writeErr(w, 404, "proxy not found")
	}
	return p
}

func validatedProxyURL(raw string) (string, error) {
	u, err := egress.Parse(raw)
	if err != nil {
		return "", err
	}
	if u == nil {
		return "", errors.New("proxy URL is required")
	}
	if u.User != nil && u.User.Username() == "***" {
		return "", errors.New("enter the complete proxy address, not a redacted address")
	}
	return u.String(), nil
}

func (s *Server) handleCreateProxy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, 400, invalidProxyPayloadMessage)
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || len([]rune(name)) > 100 {
		writeErr(w, 400, "name must contain 1 to 100 characters")
		return
	}
	endpoint, err := validatedProxyURL(body.URL)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	p := &store.Proxy{Name: name, URL: endpoint}
	if err := s.store.CreateProxy(r.Context(), p); err != nil {
		writeErr(w, 500, "could not create proxy")
		return
	}
	_ = s.store.RecordAudit(r.Context(), "proxy.created", strconv.FormatInt(p.ID, 10))
	writeJSON(w, 201, map[string]any{"proxy": viewProxy(p)})
}

func (s *Server) handleGetProxy(w http.ResponseWriter, r *http.Request) {
	if p := s.loadProxy(w, r); p != nil {
		writeJSON(w, 200, map[string]any{"proxy": viewProxy(p)})
	}
}

func (s *Server) handleUpdateProxy(w http.ResponseWriter, r *http.Request) {
	p := s.loadProxy(w, r)
	if p == nil {
		return
	}
	var body struct {
		Name *string `json:"name"`
		URL  *string `json:"url"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, 400, invalidProxyPayloadMessage)
		return
	}
	if body.Name != nil {
		p.Name = strings.TrimSpace(*body.Name)
		if p.Name == "" || len([]rune(p.Name)) > 100 {
			writeErr(w, 400, "name must contain 1 to 100 characters")
			return
		}
	}
	if body.URL != nil && strings.TrimSpace(*body.URL) != "" {
		endpoint, err := validatedProxyURL(*body.URL)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		if endpoint != p.URL {
			p.LastTest = nil
		}
		p.URL = endpoint
	}
	if err := s.store.UpdateProxy(r.Context(), p); err != nil {
		writeErr(w, 500, "could not update proxy")
		return
	}
	_ = s.store.RecordAudit(r.Context(), "proxy.updated", strconv.FormatInt(p.ID, 10))
	writeJSON(w, 200, map[string]any{"proxy": viewProxy(p)})
}

func (s *Server) handleDeleteProxy(w http.ResponseWriter, r *http.Request) {
	p := s.loadProxy(w, r)
	if p == nil {
		return
	}
	if err := s.store.DeleteProxy(r.Context(), p.ID); err != nil {
		if errors.Is(err, store.ErrProxyInUse) || strings.Contains(err.Error(), "FOREIGN KEY") {
			writeErr(w, 409, "proxy is assigned to accounts; reassign or explicitly disconnect them before deleting")
			return
		}
		writeErr(w, 500, "could not delete proxy")
		return
	}
	_ = s.store.RecordAudit(r.Context(), "proxy.deleted", strconv.FormatInt(p.ID, 10))
	writeJSON(w, 200, map[string]any{"ok": true})
}

// probeProxy uses only the egress factory. It never copies a browser header,
// account token, configured target userinfo, cookie or redirect credential.
func (s *Server) probeProxy(ctx context.Context, endpoint string) *store.ProxyTest {
	result := &store.ProxyTest{TestedAt: store.NowMS()}
	started := time.Now()
	defer func() { result.LatencyMS = time.Since(started).Milliseconds() }()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	client, err := s.factory.HTTPClient(endpoint)
	if err != nil {
		result.Error = "could not initialize proxy transport"
		return result
	}
	target, err := url.Parse(s.cfg.Upstream.BaseURL)
	if err != nil || target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") {
		result.Error = "configured test target is invalid"
		return result
	}
	target.User = nil
	target.RawQuery = ""
	target.Fragment = ""
	target.Path = strings.TrimRight(target.Path, "/") + "/"
	target.RawPath = ""
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		result.Error = "could not create connectivity request"
		return result
	}
	request.Header.Set("User-Agent", "pi-gateway-proxy-check")
	// Copy instead of mutating the factory's shared client.
	isolated := *client
	isolated.Jar = nil
	isolated.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := isolated.Do(request)
	if err != nil {
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			result.Error = "proxy connection timed out"
		} else {
			result.Error = "proxy connection failed; check address, credentials and target connectivity"
		}
		return result
	}
	defer response.Body.Close()
	result.Status = response.StatusCode
	// An unauthenticated upstream may legitimately return 401/403/404. This is a
	// connectivity check, not an account authorization or upstream health check.
	result.Success = response.StatusCode < 500 && response.StatusCode != 407
	if !result.Success {
		result.Error = fmt.Sprintf("proxy or target returned HTTP %d", response.StatusCode)
	}
	return result
}

func (s *Server) handleProbeProxy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, 400, invalidProxyPayloadMessage)
		return
	}
	endpoint, err := validatedProxyURL(body.URL)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"test": s.probeProxy(r.Context(), endpoint)})
}

func (s *Server) handleTestSavedProxy(w http.ResponseWriter, r *http.Request) {
	p := s.loadProxy(w, r)
	if p == nil {
		return
	}
	p.LastTest = s.probeProxy(r.Context(), p.URL)
	if err := s.store.UpdateProxyTest(r.Context(), p.ID, p.URL, p.LastTest); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, 409, "proxy changed during the test; please test again")
			return
		}
		writeErr(w, 500, "could not save test result")
		return
	}
	writeJSON(w, 200, map[string]any{"proxy": viewProxy(p), "test": p.LastTest})
}

func (s *Server) handleProxyAccounts(w http.ResponseWriter, r *http.Request) {
	p := s.loadProxy(w, r)
	if p == nil {
		return
	}
	page, size, err := proxyPagination(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	items, total, err := s.store.ListProxyAccounts(r.Context(), p.ID, r.URL.Query().Get("search"), size, (page-1)*size)
	if err != nil {
		writeErr(w, 500, "could not load assigned accounts")
		return
	}
	out := make([]accountView, 0, len(items))
	for _, a := range items {
		out = append(out, s.viewAccount(r.Context(), a))
	}
	writeJSON(w, 200, map[string]any{"accounts": out, "total": total, "page": page, "page_size": size})
}

func (s *Server) handleAssignProxyAccounts(w http.ResponseWriter, r *http.Request) {
	p := s.loadProxy(w, r)
	if p == nil {
		return
	}
	var body struct {
		AccountIDs *[]int64 `json:"account_ids"`
	}
	if err := decodeJSON(r, &body); err != nil || body.AccountIDs == nil {
		writeErr(w, 400, "account_ids must be an array")
		return
	}
	if len(*body.AccountIDs) > 10000 {
		writeErr(w, 400, "too many account ids")
		return
	}
	if err := s.store.SetProxyAccounts(r.Context(), p.ID, *body.AccountIDs); err != nil {
		writeErr(w, 400, "could not assign accounts; check that all selected accounts and the proxy still exist")
		return
	}
	_ = s.store.RecordAudit(r.Context(), "proxy.accounts_assigned", strconv.FormatInt(p.ID, 10))
	writeJSON(w, 200, map[string]any{"ok": true})
}

// resolveProxySelection distinguishes an omitted field from explicit null.
func (s *Server) resolveProxySelection(ctx context.Context, raw json.RawMessage) (*store.Proxy, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var id int64
	if err := json.Unmarshal(raw, &id); err != nil || id <= 0 {
		return nil, errors.New("proxy_id must be a positive integer or null")
	}
	p, err := s.store.GetProxy(ctx, id)
	if err != nil {
		return nil, errors.New("could not load selected proxy")
	}
	if p == nil {
		return nil, errors.New("selected proxy does not exist")
	}
	return p, nil
}

// bindOAuthProxy wraps the normal completion callback at server construction,
// so both pasted redirects and the local OAuth callback persist the selection.
func (s *Server) bindOAuthProxy() {
	if s.flows == nil {
		return
	}
	if s.factory != nil {
		s.flows.SetHTTPClientFactory(s.factory.HTTPClient)
	}
	s.flows.WrapCompletion(func(next func(*oauth.Flow, *oauth.Token) error) func(*oauth.Flow, *oauth.Token) error {
		return func(flow *oauth.Flow, token *oauth.Token) error {
			return s.completeOAuthWithProxy(next, flow, token)
		}
	})
}

func (s *Server) completeOAuthWithProxy(next func(*oauth.Flow, *oauth.Token) error, flow *oauth.Flow, token *oauth.Token) error {

	raw := json.RawMessage(flow.Meta["proxy_id"])
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p, err := s.resolveProxySelection(ctx, raw)
	if err != nil {
		return err
	}
	if p != nil {
		flow.Meta["proxy_url"] = p.URL
	}
	if next == nil {
		return errors.New("OAuth persistence callback unavailable")
	}
	if err := next(flow, token); err != nil {
		return err
	}
	if len(raw) > 0 && flow.DBAccountID > 0 {
		var id int64
		if p != nil {
			id = p.ID
		}
		return s.store.SetAccountProxy(ctx, flow.DBAccountID, id, flow.Meta["proxy_url"])
	}
	return nil
}
