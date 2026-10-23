// Package api serves the client-facing Responses API (SSE and WebSocket).
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"pi-gateway/internal/accounts"
	"pi-gateway/internal/capture"
	"pi-gateway/internal/codexbridge"
	"pi-gateway/internal/config"
	"pi-gateway/internal/keylimit"
	"pi-gateway/internal/middleware"
	"pi-gateway/internal/piwire"
	"pi-gateway/internal/rules"
	"pi-gateway/internal/rulescapture"
	"pi-gateway/internal/rulesruntime"
	"pi-gateway/internal/session"
	"pi-gateway/internal/settings"
	"pi-gateway/internal/store"
	"pi-gateway/internal/upstream"
)

// MaxClientBody bounds the client request body.
const MaxClientBody = 64 << 20

// Server serves the data plane.
type Server struct {
	cfg           *config.Config
	store         *store.Store
	accounts      *accounts.Manager
	upstream      *upstream.Client
	settings      *settings.Holder
	limiter       *keylimit.Limiter
	registry      map[string]middleware.Middleware
	ruleService   *rulesruntime.Service
	logger        *slog.Logger
	wsMaxAge      time.Duration
	lifecycle     wsLifecycle
	catalogCache  *modelCatalogCache
	codexRecovery *codexbridge.Recovery
}

// Options configures the data-plane server.
type Options struct {
	Config   *config.Config
	Store    *store.Store
	Accounts *accounts.Manager
	Upstream *upstream.Client
	Settings *settings.Holder
	Logger   *slog.Logger
	// CatalogCache must share the database deployment's isolated namespace.
	CatalogCache   session.CatalogCache
	CatalogTTL     time.Duration
	CatalogTimeout time.Duration
	// WSConnectionMaxAge bounds a downstream connection including active streams.
	WSConnectionMaxAge time.Duration
}

// New builds the data-plane server.
func New(opts Options) *Server {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if opts.WSConnectionMaxAge <= 0 {
		opts.WSConnectionMaxAge = time.Hour
	}
	return &Server{
		cfg:           opts.Config,
		store:         opts.Store,
		accounts:      opts.Accounts,
		upstream:      opts.Upstream,
		settings:      opts.Settings,
		limiter:       keylimit.NewLimiter(opts.Store),
		registry:      middleware.Registry(),
		ruleService:   rulesruntime.New(opts.Store),
		logger:        logger,
		wsMaxAge:      opts.WSConnectionMaxAge,
		catalogCache:  newModelCatalogCache(opts.CatalogCache, opts.CatalogTTL, opts.CatalogTimeout, logger),
		codexRecovery: codexbridge.NewRecovery(),
	}
}

// Routes registers the data-plane endpoints.
//
// /v1/responses is the canonical OpenAI Responses API path. The
// /backend-api/codex/responses alias lets Pi (and the Codex CLI) be pointed at
// this gateway by changing only their base URL, since Pi appends
// "/codex/responses" to whatever base URL it is given.
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletionsHTTP)
	for _, path := range []string{"/v1/responses/compact", "/backend-api/codex/responses/compact", "/codex/responses/compact", "/responses/compact"} {
		mux.HandleFunc("POST "+path, s.handleResponsesHTTP)
	}
	mux.HandleFunc("POST /v1/responses", s.handleResponsesHTTP)
	mux.HandleFunc("GET /v1/responses", s.handleResponsesWS)
	mux.HandleFunc("POST /backend-api/codex/responses", s.handleResponsesHTTP)
	mux.HandleFunc("GET /backend-api/codex/responses", s.handleResponsesWS)
	mux.HandleFunc("POST /codex/responses", s.handleResponsesHTTP)
	mux.HandleFunc("GET /codex/responses", s.handleResponsesWS)
	mux.HandleFunc("POST /responses", s.handleResponsesHTTP)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /healthz", s.handleHealth)
}

// prepared is one fully validated, middleware-processed request.
type prepared struct {
	CompactionMode     string
	CompactionModel    string
	CompactionPrompt   string
	Compact            bool
	CompactDirect      bool
	Key                *store.APIKey
	Account            *store.Account
	Release            func()
	AccountRelease     func()
	ClientBody         map[string]any
	ClientHeaders      http.Header
	Built              *piwire.BuiltRequest
	SessionID          string // Internal session, always set for upstream connection pooling.
	WireSessionID      string // Only the client's explicit session hints reach the wire.
	PoolSessionID      string // Opaque downstream scope; never sent on the wire.
	PreviousResponseID string
	Continuation       *session.Record
	CodexBridge        codexbridge.Profile // Bounded protocol metadata; never task content.
	CodexRecovery      *codexbridge.Recovery
	CodexRecoveryScope string
	ResponseHeaders    http.Header // Safe upstream retry metadata, independent of capture.
	Transport          string
	Recorder           *capture.Recorder
	// Usage is the billing/statistics ledger entry; it is recorded even when
	// diagnostic capture is off.
	Usage *usageTracker
	MW    middleware.Result
	// RuleEngine pins the same immutable snapshot for request and response stages.
	RuleEngine   *rules.Engine
	RuleVersion  int64
	RuleContext  map[string]any
	RuleEventSeq int64
	// WantsStream reports whether the client asked for SSE.
	WantsStream bool
	// RawRequestBody preserves the client's original bytes for diagnostics.
	RawRequestBody []byte
}

// prepare pins the rule snapshot before admission and retains diagnostic state
// for every later early return, including requests which never select an account.
func (s *Server) prepare(ctx context.Context, r *http.Request, rawBody []byte, clientTransport string) (*prepared, *apiError) {
	return s.prepareInput(ctx, r, rawBody, rawBody, clientTransport)
}

// prepareInput keeps the original client payload for diagnostics and rule facts,
// while protocol adapters supply a Responses payload to the shared pipeline.
func (s *Server) prepareInput(ctx context.Context, r *http.Request, rawBody, responsesBody []byte, clientTransport string) (*prepared, *apiError) {
	key, err := s.authenticate(ctx, r)
	if err != nil {
		return nil, &apiError{Status: http.StatusUnauthorized, Message: "invalid or missing API key", Type: "invalid_request_error"}
	}
	clientBody, bodyErr := decodeClientBody(rawBody)
	if bodyErr != nil {
		return nil, bodyErr
	}
	responseInput := clientBody
	if !bytes.Equal(rawBody, responsesBody) {
		responseInput, bodyErr = decodeClientBody(responsesBody)
		if bodyErr != nil {
			return nil, bodyErr
		}
	}
	compactDirect := strings.HasSuffix(r.URL.Path, "/responses/compact")
	compact, compactErr := detectCompaction(responseInput, compactDirect)
	if compactErr != nil {
		return nil, compactErr
	}
	rt := s.settings.Get()
	headerHints := []string{r.Header.Get("x-client-request-id"), r.Header.Get("session-id"), r.Header.Get("session_id"), r.Header.Get("x-session-id")}
	sessionID := piwire.DeriveSessionID(responseInput, headerHints, func() string { return internalSessionID(ctx) })
	promptCacheKey := piwire.DeriveSessionID(responseInput, headerHints, nil)
	normalizeBody, expandErr := s.expandClientCompaction(responsesBody, responseInput, key.ID)
	if expandErr != nil {
		return nil, expandErr
	}
	engine, version, err := s.ruleService.Load(ctx)
	if err != nil && !errors.Is(err, rulesruntime.ErrMigration) {
		return nil, ruleAPIError("could not load rules")
	}
	built := &piwire.BuiltRequest{}
	p := &prepared{
		CompactionMode: rt.CompactionMode, CompactionModel: rt.CompactionModel, CompactionPrompt: rt.CompactionPrompt, Compact: compact, CompactDirect: compactDirect,
		RuleEngine: engine, RuleVersion: version, Key: key, ClientBody: clientBody, ClientHeaders: r.Header.Clone(), Built: built, SessionID: sessionID,
		WireSessionID: promptCacheKey, PoolSessionID: poolSessionID(ctx, sessionID),
		WantsStream: wantsStream(responseInput), RawRequestBody: rawBody,
		RuleContext: map[string]any{"original_model": clientBody["model"], "model": clientBody["model"], "settings": s.ruleSettings(rt), "wire_session_id": promptCacheKey, "client_headers": rules.RedactedHeaders(r.Header),
			"compact": compact, "api_key_id": key.ID, "client_protocol": clientTransport,
			"request_path": r.URL.Path, "request_method": r.Method},
	}
	if compactDirect {
		p.WantsStream = false
	}
	if rt.CaptureEnabled {
		p.Recorder = s.newRecorder(key, nil, clientTransport, "", built)
		p.Recorder.OnClientRequest(r.Header, rawBody)
	}
	if err := s.normalizePrepared(ctx, p, normalizeBody, rt, promptCacheKey); err != nil {
		return p, err
	}
	p.shapeLegacyCompaction(normalizeBody)
	if apiErr := s.applyRequestRules(ctx, p); apiErr != nil {
		return p, apiErr
	}
	// Capture tool identity before compact finalization removes the tools table.
	if apiErr := s.detectCodexBridge(p); apiErr != nil {
		return p, apiErr
	}
	if err := p.shapePrepared(ctx); err != nil {
		return p, err
	}
	if err := s.resolvePreparedContinuation(ctx, p); err != nil {
		return p, err
	}
	if apiErr := s.prepareCodexBridge(p); apiErr != nil {
		return p, apiErr
	}

	if err := s.routePrepared(ctx, p, rt, clientTransport, promptCacheKey); err != nil {
		return p, err
	}
	return p, nil
}

// resolveTransport decides the upstream transport.
//
// The account's upstream_protocol (sse | ws) is authoritative: it selects the
// protocol used *towards the backend*, independent of what the client chose. When
// the account has no override, the key's then the global default apply; the global
// default is SSE, so the client's own protocol never decides the upstream protocol
// unless an operator explicitly asks for passthrough.
func (s *Server) resolveTransport(rt *store.Settings, key *store.APIKey, account *store.Account, clientTransport string) string {
	if account != nil {
		switch strings.ToLower(strings.TrimSpace(account.UpstreamProtocol)) {
		case "ws", "websocket", cachedWebSocketTransport:
			// An account-level WS override always selects the pooled/cached socket
			// so the incremental-context path (previous_response_id + delta input)
			// is used. The upstream client only understands these internal names,
			// so "ws" must never be passed through verbatim.
			return cachedWebSocketTransport
		case "sse":
			return "sse"
		}
	}
	if key != nil && strings.TrimSpace(key.Transport) != "" {
		return normalizeTransport(key.Transport, clientTransport)
	}
	return normalizeTransport(rt.UpstreamTransport, clientTransport)
}

func normalizeTransport(configured, clientTransport string) string {
	switch strings.ToLower(strings.TrimSpace(configured)) {
	case "passthrough", "":
		if clientTransport == "ws" {
			return "websocket"
		}
		return "sse"
	case "auto":
		if clientTransport == "ws" {
			return cachedWebSocketTransport
		}
		return "auto"
	default:
		return configured
	}
}

// extraFieldsFrom is retained for compatibility; normalisation now passes through
// all client options except documented unsupported fields by default.
func extraFieldsFrom(rt *store.Settings) []string { return nil }

func wantsStream(body map[string]any) bool {
	v, ok := body["stream"]
	if !ok {
		// The Responses API streams by default for these clients.
		return true
	}
	b, ok := v.(bool)
	if !ok {
		return true
	}
	return b
}

func (s *Server) buildChain(ctx context.Context) (*middleware.Chain, error) {
	rows, err := s.store.ListMiddlewares(ctx)
	if err != nil {
		return nil, err
	}
	regs := make([]middleware.Registration, 0, len(rows))
	for _, row := range rows {
		regs = append(regs, middleware.Registration{
			Name:    row.Name,
			Enabled: row.Enabled,
			Order:   row.OrderIndex,
			Config:  row.Config,
		})
	}
	return middleware.NewChain(regs, s.registry)
}

func (s *Server) newRecorder(key *store.APIKey, account *store.Account, clientTransport, upstreamTransport string, built *piwire.BuiltRequest) *capture.Recorder {
	scaffold := &store.Capture{
		ClientTransport: clientTransport, UpstreamTransport: upstreamTransport,
		Outcome: store.OutcomePending,
	}
	if key != nil {
		scaffold.APIKeyID, scaffold.APIKeyName = key.ID, key.Name
	}
	if account != nil {
		scaffold.AccountID, scaffold.AccountName = account.ID, account.Name
	}
	if built != nil {
		scaffold.Model, scaffold.SessionID = built.Model, built.SessionID
	}
	// RequestBytes/RequestBody describe actual upstream traffic, not a prepared
	// payload which may be blocked before an account or connection is acquired.
	return capture.New(scaffold, capture.Options{
		MaxBytesPerRecord: s.cfg.Capture.MaxBytesPerRecord,
		IncludeHeaders:    s.cfg.Capture.IncludeHeaders,
	})
}

// sink collects retry headers even when diagnostic capture is disabled.
func (p *prepared) sink() upstream.Sink {
	var sink upstream.Sink = upstream.NopSink{}
	if p == nil {
		return sink
	}
	if p.Recorder != nil {
		sink = p.Recorder
	}
	sink = &responseHeaderSink{Sink: sink, prepared: p}
	if p.Usage != nil {
		sink = &usageSink{Sink: sink, t: p.Usage}
	}
	return sink
}

// Upstream invokes sink callbacks and event callbacks on the stream goroutine.
// Embedding preserves all capture callbacks without coupling retries to capture.
type responseHeaderSink struct {
	upstream.Sink
	prepared *prepared
}

func (s *responseHeaderSink) OnResponseHeaders(status int, headers http.Header) {
	// Replace rather than merge: a retry/fallback response supersedes its predecessor.
	s.prepared.ResponseHeaders = safeRetryHeaders(headers)
	s.Sink.OnResponseHeaders(status, headers)
}

// Observe the successful wire protocol, not the configured routing mode. This
// also handles cached WebSockets and auto fallback with capture disabled.
func (s *responseHeaderSink) OnFrame(dir, kind, eventType string, raw []byte, parsed map[string]any) {
	if dir == "in" && s.prepared.RuleContext != nil {
		switch kind {
		case store.KindSSEEvent:
			s.prepared.RuleContext["upstream_protocol"] = "sse"
		case store.KindWSFrame:
			s.prepared.RuleContext["upstream_protocol"] = "ws"
		}
	}
	s.Sink.OnFrame(dir, kind, eventType, raw, parsed)
}

// finish persists the capture, releasing the account slot.
func (s *Server) finish(ctx context.Context, p *prepared, outcome string, err error) {
	if p == nil {
		return
	}
	if p.Release != nil {
		defer p.Release()
	}

	if p.Recorder != nil {
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		c := p.Recorder.Finalize(outcome, msg)
		rt := s.settings.Get()
		limit := rt.CaptureLimit
		if s.cfg.Capture.Persist {
			pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			if err := s.store.InsertCapture(pctx, c, limit); err != nil {
				s.logger.Warn("persisting capture failed", "error", err)
			}
		}
	}

	if p.Usage != nil {
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		p.Usage.finalize(ctx, outcome, msg)
	}

	if p.Account != nil {
		failed := outcome != store.OutcomeOK
		mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := s.store.MarkAccountUsed(mctx, p.Account.ID, failed); err != nil {
			s.logger.Debug("marking account used failed", "error", err)
		}
		if p.Key != nil {
			if err := s.store.MarkKeyUsed(mctx, p.Key.ID); err != nil {
				s.logger.Debug("marking key used failed", "error", err)
			}
		}
	}
}

// ── response helpers ──

// apiError is a client-facing error.
type apiError struct {
	Status          int
	Message         string
	Type            string
	Code            string
	RequestID       string
	ResponseHeaders http.Header
}

func (e *apiError) Error() string { return e.Message }

// writeError renders an OpenAI-style error envelope.
func writeError(w http.ResponseWriter, e *apiError) {
	if e == nil {
		e = &apiError{Status: http.StatusInternalServerError, Message: "internal error", Type: "server_error"}
	}
	copyResponseHeaders(w, e.ResponseHeaders)
	if e.RequestID != "" && w.Header().Get(requestIDHeader) == "" {
		w.Header().Set(requestIDHeader, e.RequestID)
	}
	inner := map[string]any{
		"message": e.Message,
		"type":    orDefault(e.Type, "server_error"),
		"code":    orDefault(e.Code, nil),
	}
	if e.RequestID != "" {
		inner["request_id"] = e.RequestID
	}
	payload := map[string]any{"error": inner}
	writeJSON(w, e.Status, payload)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	_ = enc.Encode(payload)
}

func orDefault(v, fallback any) any {
	switch t := v.(type) {
	case string:
		if strings.TrimSpace(t) == "" {
			return fallback
		}
	}
	if v == nil {
		return fallback
	}
	return v
}

// errorFromUpstream maps an upstream failure to a client status code.
func errorFromUpstream(err error, responseHeaders ...http.Header) (apiErr *apiError) {
	if err == nil {
		return nil
	}
	defer func() {
		apiErr.ResponseHeaders = safeRetryHeaders(responseHeaders...)
		if apiErr.RequestID == "" {
			apiErr.RequestID = apiErr.ResponseHeaders.Get(requestIDHeader)
		} else if apiErr.ResponseHeaders.Get(requestIDHeader) == "" {
			apiErr.ResponseHeaders.Set(requestIDHeader, apiErr.RequestID)
		}
	}()
	var policyError *apiError
	if errors.As(err, &policyError) {
		cloned := *policyError
		return &cloned
	}
	if errors.Is(err, upstream.ErrContinuationUnavailable) {
		return continuationAPIError(err)
	}
	if errors.Is(err, upstream.ErrPoolBusy) {
		return &apiError{Status: http.StatusTooManyRequests, Type: "rate_limit_error", Code: "websocket_pool_busy", Message: "WebSocket connection capacity or wait queue is full."}
	}
	if errors.Is(err, upstream.ErrWSMaxAge) {
		return &apiError{Status: http.StatusBadRequest, Type: "invalid_request_error", Code: "websocket_connection_limit_reached", Message: "The upstream WebSocket reached its maximum age. Start a new chain with complete input."}
	}
	var failure *upstream.FailureError
	if errors.As(err, &failure) {
		return failureAPIError(failure)
	}
	if errors.Is(err, context.Canceled) {
		return &apiError{Status: 499, Message: "client closed request", Type: "cancelled", Code: "client_closed_request"}
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || upstream.IsIdleTimeoutError(err) ||
		(errors.As(err, &netErr) && netErr.Timeout()) {
		return &apiError{Status: http.StatusGatewayTimeout, Message: "upstream timed out", Type: "timeout", Code: "upstream_timeout"}
	}
	return &apiError{Status: http.StatusBadGateway, Message: err.Error(), Type: "upstream_error", Code: "upstream_transport_error"}
}

// streamErrorPayload renders a mid-stream failure as the protocol's structured
// error event (codex-proxy-rs shape) so clients receive a typed failure with a
// status and code instead of a bare message string.
func streamErrorPayload(err error, responseHeaders ...http.Header) map[string]any {
	apiErr := errorFromUpstream(err, responseHeaders...)
	if apiErr == nil {
		apiErr = &apiError{Status: http.StatusBadGateway, Message: "upstream error", Type: "upstream_error"}
	}
	inner := map[string]any{
		"type":    orDefault(apiErr.Type, "upstream_error"),
		"message": apiErr.Message,
	}
	if apiErr.Code != "" {
		inner["code"] = apiErr.Code
	}
	if apiErr.RequestID != "" {
		inner["request_id"] = apiErr.RequestID
	}
	payload := map[string]any{
		"type":   "error",
		"status": apiErr.Status,
		"error":  inner,
	}
	addRetryHeadersToPayload(payload, apiErr.ResponseHeaders)
	return payload
}

// errorFramePayload renders an apiError as a structured error frame.
func errorFramePayload(e *apiError) map[string]any {
	if e == nil {
		return streamErrorPayload(nil)
	}
	inner := map[string]any{
		"type":    orDefault(e.Type, "invalid_request_error"),
		"message": e.Message,
	}
	if e.Code != "" {
		inner["code"] = e.Code
	}
	if e.RequestID != "" {
		inner["request_id"] = e.RequestID
	}
	payload := map[string]any{
		"type":   "error",
		"status": e.Status,
		"error":  inner,
	}
	addRetryHeadersToPayload(payload, e.ResponseHeaders)
	return payload
}

// A committed SSE response or upgraded socket cannot acquire new HTTP headers.
// Keep retry metadata in the structured error event for those clients instead.
func addRetryHeadersToPayload(payload map[string]any, headers http.Header) {
	values := map[string]string{}
	for name, entries := range safeRetryHeaders(headers) {
		if len(entries) > 0 {
			values[strings.ToLower(name)] = entries[0]
		}
	}
	if len(values) > 0 {
		payload["headers"] = values
	}
}

// readBody reads and bounds the client body.
func readBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	limited := io.LimitReader(r.Body, MaxClientBody+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > MaxClientBody {
		return nil, fmt.Errorf("request body exceeds %d bytes", MaxClientBody)
	}
	return body, nil
}

// handleHealth reports liveness plus basic upstream/pool state.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	accounts, err := s.store.ListAccounts(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"status": "degraded", "error": err.Error()})
		return
	}
	enabled := 0
	for _, a := range accounts {
		if a.Enabled {
			enabled++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":             "ok",
		"accounts":           len(accounts),
		"accounts_enabled":   enabled,
		"websocket_pooled":   s.upstream.PoolSize(),
		"upstream_transport": s.settings.Get().UpstreamTransport,
	})
}

var publicModelKeyNormalizer = strings.NewReplacer("_", "", "-", "", ".", "")

// publicModelMetadata keeps unknown capability fields while removing private
// account/credential fields if an upstream model entry happens to contain them.
// Never mutate the cached metadata; aliases share their target's capabilities.
func publicModelMetadata(metadata map[string]json.RawMessage) map[string]json.RawMessage {
	if metadata == nil {
		return nil
	}
	public := make(map[string]json.RawMessage, len(metadata))
	for key, value := range metadata {
		normalized := publicModelKeyNormalizer.Replace(strings.ToLower(key))
		switch normalized {
		case "account", "accounts", "accountid", "accountids", "organizationid", "userid", "email",
			"accesstoken", "refreshtoken", "idtoken", "token", "apikey", "apikeys", "credential", "credentials",
			"password", "secret", "clientsecret", "authorization", "proxyauthorization", "header", "headers",
			"bearer", "bearertoken", "xapikey", "xauthtoken",
			"requestheaders", "responseheaders", "cookie", "cookies", "setcookie", "proxyurl":
			continue
		}
		public[key] = publicModelMetadataValue(value)
	}
	return public
}

func publicModelMetadataValue(value json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(value)
	if len(trimmed) == 0 {
		return value
	}
	switch trimmed[0] {
	case '{':
		var object map[string]json.RawMessage
		if json.Unmarshal(value, &object) == nil && object != nil {
			raw, _ := json.Marshal(publicModelMetadata(object))
			return raw
		}
	case '[':
		var array []json.RawMessage
		if json.Unmarshal(value, &array) == nil && array != nil {
			for i := range array {
				array[i] = publicModelMetadataValue(array[i])
			}
			raw, _ := json.Marshal(array)
			return raw
		}
	}
	// Scalar capabilities already are raw JSON; preserve numbers and explicit
	// zero values without two failed object/array decoder allocations per field.
	return value
}

// publicModelEntry projects only a model entry, not its catalog envelope. Raw
// messages preserve unknown capabilities, explicit zero values and large numbers.
func publicModelEntry(model store.CatalogModel, id string, codex bool) map[string]json.RawMessage {
	entry := publicModelMetadata(model.Metadata)
	if entry == nil {
		entry = make(map[string]json.RawMessage)
	}
	setString := func(key, value string) {
		entry[key], _ = json.Marshal(value)
	}
	defaultString := func(key, value string) {
		if _, exists := entry[key]; !exists {
			setString(key, value)
		}
	}
	setString("id", id)
	if _, hasSlug := entry["slug"]; hasSlug || codex {
		setString("slug", id)
	}
	var modelID string
	if json.Unmarshal(entry["model"], &modelID) == nil && modelID == model.ID {
		setString("model", id)
	}
	defaultString("object", "model")
	defaultString("owned_by", "openai")
	name := model.Name
	if strings.TrimSpace(name) == "" {
		name = model.ID
	}
	defaultString("name", name)
	if codex {
		defaultString("display_name", name)
	}
	if model.Description != "" {
		defaultString("description", model.Description)
	}
	if model.Source != "" {
		defaultString("source", model.Source)
	}
	delete(entry, "origins") // Provider provenance belongs to the gateway, not raw metadata.
	if origins := mergedCatalogOrigins(model.Origins); len(origins) > 0 {
		entry["origins"], _ = json.Marshal(origins)
	}
	return entry
}

// mergeCatalogModel prefers observed upstream capabilities over manual defaults.
// Callers supply accounts in ID order. Within the same source, only absent keys
// are filled; conflicting values and arrays never become an invented union.
func mergeCatalogModel(first, next store.CatalogModel) store.CatalogModel {
	if first.Source == "manual" && next.Source != "manual" {
		return next
	}
	if first.Source != next.Source {
		return first
	}
	metadata := make(map[string]json.RawMessage, len(first.Metadata)+len(next.Metadata))
	for key, value := range first.Metadata {
		metadata[key] = value
	}
	for key, value := range next.Metadata {
		if _, exists := metadata[key]; !exists {
			metadata[key] = value
		}
	}
	first.Metadata = metadata
	first.Origins = mergedCatalogOrigins(first.Origins, next.Origins)
	return first
}

// Keep provider provenance separate from source-account names and never expose
// arbitrary strings inserted into a corrupt cache as a trusted provider label.
func mergedCatalogOrigins(groups ...[]string) []string {
	seen := make(map[string]bool, 2)
	for _, group := range groups {
		for _, origin := range group {
			seen[origin] = true
		}
	}
	var origins []string
	for _, source := range []string{store.ModelSourceChatGPT, store.ModelSourceCodex} {
		if seen[source] {
			origins = append(origins, source)
		}
	}
	return origins
}

func validModelClientVersion(version string) bool {
	if len(version) > 64 {
		return false
	}
	for _, c := range version {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '+') {
			return false
		}
	}
	return true
}

// handleModels advertises reachable accounts' upstream and manually supplemented
// models. Listing a model is not a guarantee that generation succeeds.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	key, err := s.authenticate(r.Context(), r)
	if err != nil {
		writeError(w, &apiError{Status: http.StatusUnauthorized, Message: "invalid or missing API key", Type: "invalid_request_error"})
		return
	}
	candidates, err := s.store.EligibleAccountsForKey(r.Context(), key)
	if err != nil {
		writeError(w, &apiError{Status: http.StatusInternalServerError, Message: "could not load the account model scope", Type: "server_error"})
		return
	}
	query, queryErr := url.ParseQuery(r.URL.RawQuery)
	clientVersion := query.Get("client_version")
	if queryErr != nil || len(query["client_version"]) > 1 || !validModelClientVersion(clientVersion) {
		writeError(w, &apiError{Status: http.StatusBadRequest, Message: "invalid client_version", Type: "invalid_request_error"})
		return
	}
	codexMode := clientVersion != ""
	// Account selection order is independent of database row order or scheduling
	// priority. Real upstream entries win over manual defaults; same-source
	// duplicates are deterministic by account ID and only fill absent metadata.
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	available := map[string]store.CatalogModel{}
	observed := map[string]bool{}
	accountIDs := modelCatalogAccountIDs(candidates)
	// Neither current permissions nor final key-filtered responses are cached.
	restrictions, policyErr := s.store.DisabledModelsForAccounts(r.Context(), accountIDs, key)
	if policyErr != nil {
		writeError(w, &apiError{Status: http.StatusInternalServerError, Message: "could not load model restrictions", Type: "server_error"})
		return
	}
	catalogs, loadErr := s.loadModelCatalogs(r.Context(), accountIDs)
	if loadErr != nil {
		writeError(w, &apiError{Status: http.StatusInternalServerError, Message: "could not load an account model catalog", Type: "server_error"})
		return
	}
	mergeVisibleCatalogs(accountIDs, restrictions, catalogs, available, observed)
	// Resolve mappings once, exactly as piwire.BuildRequest does. Even a native
	// model name must be omitted if configuration remaps it to a blocked target.
	mappings := s.settings.Get().ModelMappings
	observeModelAliases(mappings, observed)
	ids := make([]string, 0, len(observed))
	for id := range observed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	data := visibleModelEntries(ids, mappings, available, codexMode)
	if len(data) == 0 {
		w.Header().Set("X-Model-Catalog-Status", "empty")
	} else {
		w.Header().Set("X-Model-Catalog-Status", "cached")
	}
	if codexMode {
		writeJSON(w, http.StatusOK, map[string]any{"models": data})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) normalizePrepared(ctx context.Context, p *prepared, normalizeBody map[string]any, rt *store.Settings, promptCacheKey string) *apiError {
	engine, built := p.RuleEngine, p.Built
	var err error
	if engine != nil {
		initial, err := p.applyPipeline(ctx, rules.PhaseClientRequest, normalizeBody)
		if err != nil {
			return pipelineAPIError(err)
		}
		normalized, err := p.applyPipeline(ctx, rules.PhaseRequestNormalize, initial.Body)
		if err != nil {
			return pipelineAPIError(err)
		}
		built.Body, err = ruleOrderedBody(normalized.Body, nil)
		if err != nil {
			return ruleAPIError(err.Error())
		}
		m, _ := built.Body.Get("model")
		built.Model, _ = m.(string)
		if strings.TrimSpace(built.Model) == "" {
			return &apiError{Status: 400, Message: "no model specified and no default model configured", Type: "invalid_request_error"}
		}
		built.JSON, err = json.Marshal(built.Body)
		if err != nil {
			return ruleAPIError(err.Error())
		}
		p.RuleContext["model"] = built.Model
	} else {
		built, err = rulescapture.NormalizeRequest(normalizeBody, rt, promptCacheKey)
		if err != nil {
			return ruleAPIError(err.Error())
		}
		p.Built = built
	}
	return nil
}

func (p *prepared) shapeLegacyCompaction(normalizeBody map[string]any) {
	compact, engine, built := p.Compact, p.RuleEngine, p.Built
	if compact && engine == nil {
		for _, field := range []string{"instructions", "prompt_cache_options"} {
			if value, ok := normalizeBody[field]; ok {
				built.Body.Set(field, value)
			}
		}
		built.JSON, _ = json.Marshal(built.Body)
	}
	if compact && engine == nil && p.CompactionMode == "on" {
		before, _ := json.Marshal(built.Body)
		built.Body.Set("model", p.CompactionModel)
		built.Model = p.CompactionModel
		built.JSON, _ = json.Marshal(built.Body)
		p.RuleContext["model"] = built.Model
		p.recordGatewayDifference("compaction_model", "Model summary configuration", before, built.JSON)
	}
}

func (p *prepared) shapePrepared(ctx context.Context) *apiError {
	engine, built := p.RuleEngine, p.Built
	var err error
	beforeShape, _ := json.Marshal(built.Body)
	if engine != nil {
		if err := p.finalizePreparedRules(ctx); err != nil {
			return err
		}
	} else if p.Compact {
		built.Body, err = compactRequestBody(built.Body)
		if err != nil {
			return compactInputError(err.Error())
		}
		built.JSON, err = json.Marshal(built.Body)
		if err != nil {
			return ruleAPIError(err.Error())
		}
	} else {
		piwire.EnforcePiShape(built.Body)
		built.JSON, err = json.Marshal(built.Body)
		if err != nil {
			return ruleAPIError(err.Error())
		}
	}
	p.recordCompactionShape(beforeShape)
	return nil
}

func (s *Server) resolvePreparedContinuation(ctx context.Context, p *prepared) *apiError {
	built, sessionID, key := p.Built, p.SessionID, p.Key
	built.SessionID = sessionID
	rawPrevious, _ := built.Body.Get("previous_response_id")
	continuation, previousID, continuationErr := s.resolveContinuation(ctx, key.ID, rawPrevious)
	if continuationErr != nil {
		return continuationErr
	}
	if continuation != nil {
		sessionID = continuation.SessionID
		built.SessionID = sessionID
	}
	p.SessionID, p.PreviousResponseID, p.Continuation = sessionID, previousID, continuation
	p.PoolSessionID = poolSessionID(ctx, sessionID)
	return nil
}

func (s *Server) routePrepared(ctx context.Context, p *prepared, rt *store.Settings, clientTransport, promptCacheKey string) *apiError {
	key, built, continuation, sessionID, clientBody := p.Key, p.Built, p.Continuation, p.SessionID, p.ClientBody
	keyRelease, admissionErr := s.admitPreparedKey(ctx, key)
	if admissionErr != nil {
		return admissionErr
	}
	account, release, err := s.acquirePreparedAccount(ctx, p, promptCacheKey)
	if err != nil {
		keyRelease()
		if continuation != nil && errors.Is(err, accounts.ErrNoAccounts) {
			return continuationAPIError(session.ErrMiss)
		}
		if errors.Is(err, accounts.ErrModelUnavailable) {
			return &apiError{Status: http.StatusForbidden, Message: "This model is disabled for the eligible accounts or groups.", Type: "invalid_request_error", Code: "model_disabled"}
		}
		return &apiError{Status: http.StatusServiceUnavailable, Message: err.Error(), Type: "server_error"}
	}
	p.AccountRelease = release
	release = composeRelease(keyRelease, release)
	transport := s.resolveTransport(rt, key, account, clientTransport)
	if p.Compact {
		transport = "sse" // Compaction is a separate HTTP operation, never a socket delta.
	}
	if continuation != nil && transport != cachedWebSocketTransport && transport != "auto" {
		release()
		return continuationAPIError(session.ErrMiss)
	}
	p.Account, p.Release, p.Transport = account, release, transport
	p.RuleContext["account_id"], p.RuleContext["upstream_protocol"] = account.ID, transport
	p.RuleContext["model"] = built.Model
	if p.Recorder != nil {
		p.Recorder.SetRoute(account.ID, account.Name, built.Model, sessionID, transport)
	}
	p.Usage = s.newUsageTracker(key, account, built.Model, clientTransport, transport, sessionID, stringField(clientBody, "service_tier"))
	p.Usage.noteRequestMetadata(built.JSON)
	if p.Compact {
		p.Usage.requestKind = "compaction"
	}
	s.startUsage(ctx, p.Usage)
	return nil
}

func (s *Server) admitPreparedKey(ctx context.Context, key *store.APIKey) (func(), *apiError) {
	keyRelease := func() {
		// Requests without key admission have no reservation to release.
	}
	if key != nil {
		dec, rel, lerr := s.limiter.Admit(ctx, key.ID, keylimit.LimitsFromKey(key), time.Now())
		if lerr != nil {
			s.logger.Warn("key admission check failed; allowing request", "error", lerr, "key_id", key.ID)
		} else {
			if !dec.Allowed {
				return nil, &apiError{Status: http.StatusTooManyRequests, Message: admissionMessage(dec), Type: "rate_limit_error", Code: dec.Reason, ResponseHeaders: retryAfterHeaders(dec.RetryAfter)}
			}

			keyRelease = rel

		}
	}

	return keyRelease, nil
}

func (s *Server) acquirePreparedAccount(ctx context.Context, p *prepared, promptCacheKey string) (*store.Account, func(), error) {
	key, built, continuation, sessionID := p.Key, p.Built, p.Continuation, p.SessionID
	var err error
	var account *store.Account
	var release func()
	if continuation != nil {
		account, release, err = s.accounts.AcquirePinned(ctx, key, continuation.AccountID, built.Model)
	} else {
		scope := ""
		connection, _ := ctx.Value(downstreamWSContextKey{}).(string)
		if connection != "" || promptCacheKey != "" {
			scope = poolSessionID(ctx, sessionID)
		}
		account, release, err = s.accounts.AcquireWithAffinity(ctx, key, built.Model, scope)
	}
	return account, release, err
}

func decodeClientBody(rawBody []byte) (map[string]any, *apiError) {
	var clientBody map[string]any
	if len(rawBody) > 0 {
		if err := json.Unmarshal(rawBody, &clientBody); err != nil {
			return nil, &apiError{Status: http.StatusBadRequest, Message: "request body is not valid JSON: " + err.Error(), Type: "invalid_request_error"}
		}
	}
	if clientBody == nil {
		clientBody = map[string]any{}
	}

	return clientBody, nil
}

func (s *Server) expandClientCompaction(rawBody []byte, clientBody map[string]any, keyID int64) (map[string]any, *apiError) {
	expandedBody, expandErr := s.upstream.ExpandCompactionBody(rawBody, keyID)
	if expandErr != nil {
		return nil, errorFromUpstream(expandErr)
	}
	normalizeBody := clientBody
	if !bytes.Equal(expandedBody, rawBody) {
		normalizeBody = make(map[string]any)
		if err := json.Unmarshal(expandedBody, &normalizeBody); err != nil {
			return nil, compactInputError(err.Error())
		}
	}

	return normalizeBody, nil
}

func (p *prepared) finalizePreparedRules(ctx context.Context) *apiError {
	built := p.Built
	if p.Compact {
		if _, err := compactRequestBody(built.Body); err != nil {
			return compactInputError(err.Error())
		}
	}
	if err := p.finalizeRules(ctx); err != nil {
		return pipelineAPIError(err)
	}
	return nil
}

func failureAPIError(failure *upstream.FailureError) *apiError {
	status := http.StatusBadGateway
	// HTTP status parsed from the upstream response is authoritative even when
	// its logical code does not use the http_* naming convention.
	if failure.Status >= 400 && failure.Status < 600 {
		status = failure.Status
	} else {
		switch failure.Code {
		case "http_401":
			status = http.StatusUnauthorized
		case "http_403":
			status = http.StatusForbidden
		case "http_404":
			status = http.StatusNotFound
		case "http_429", "usage_limit_reached", "usage_not_included", "rate_limit_exceeded":
			status = http.StatusTooManyRequests
		case "http_400", "invalid_request_error":
			status = http.StatusBadRequest
		}
		if strings.HasPrefix(failure.Code, "http_") {
			var parsed int
			if _, scanErr := fmt.Sscanf(failure.Code, "http_%d", &parsed); scanErr == nil && parsed >= 400 && parsed < 600 {
				status = parsed
			}
		}
	}
	return &apiError{Status: status, Message: failure.Message, Type: "upstream_error", Code: failure.Code, RequestID: failure.RequestID}
}

func modelCatalogAccountIDs(candidates []*store.Account) []int64 {
	accountIDs := make([]int64, 0, len(candidates))
	now := store.NowMS()
	for _, account := range candidates {
		if account.CooldownUntil > now {
			continue
		}
		switch account.Status {
		case store.AccountStatusExpired, store.AccountStatusInvalid, store.AccountStatusBanned, "quota_exhausted":
			continue
		}
		accountIDs = append(accountIDs, account.ID)
	}

	return accountIDs
}

func mergeVisibleCatalogs(accountIDs []int64, restrictions map[int64][]string, catalogs map[int64][]store.CatalogModel, available map[string]store.CatalogModel, observed map[string]bool) {
	for _, accountID := range accountIDs {
		disabled := restrictions[accountID]
		blocked := make(map[string]bool, len(disabled))
		for _, id := range disabled {
			blocked[id] = true
		}
		for _, model := range catalogs[accountID] {
			id := strings.TrimSpace(model.ID)
			if id == "" {
				continue
			}
			observed[id] = true
			if blocked[id] {
				continue
			}
			model.ID = id
			if first, exists := available[id]; exists {
				model = mergeCatalogModel(first, model)
			}
			available[id] = model
		}
	}
}

func (p *prepared) recordCompactionShape(beforeShape []byte) {
	compact, built := p.Compact, p.Built
	if compact && p.CompactionMode == "on" {
		p.CompactionModel = built.Model
	}
	if p.Compact {
		name := "Native compaction request"
		if p.CompactionMode == "on" {
			name = "Model summary compaction request"
		}
		p.recordGatewayDifference("compaction", name, beforeShape, built.JSON)
	}
}

func observeModelAliases(mappings map[string]string, observed map[string]bool) {
	for alias := range mappings {
		if strings.TrimSpace(alias) != "" {
			observed[alias] = true
		}
	}
}

func visibleModelEntries(ids []string, mappings map[string]string, available map[string]store.CatalogModel, codexMode bool) []map[string]json.RawMessage {
	data := make([]map[string]json.RawMessage, 0, len(ids))
	for _, id := range ids {
		target := id
		if mapped, ok := mappings[id]; ok && mapped != "" {
			target = mapped
		}
		if model, ok := available[target]; ok {
			data = append(data, publicModelEntry(model, id, codexMode))
		}
	}
	return data
}
