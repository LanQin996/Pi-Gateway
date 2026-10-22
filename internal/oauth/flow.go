package oauth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Flow status values.
const (
	FlowPending    = "pending"
	FlowExchanging = "exchanging"
	FlowCompleted  = "completed"
	FlowFailed     = "failed"
)

// Flow kinds: which OAuth credential the flow authorizes.
const (
	// FlowKindChatGPT is Sign in with ChatGPT — the credential used for model access.
	FlowKindChatGPT = "chatgpt"
	// FlowKindCodex is the legacy Codex flow — its credential reads ChatGPT quota
	// (windows, reset credits) and synchronizes the Codex model catalog, not generation.
	FlowKindCodex = "codex"
)

// Flow is one in-progress OAuth authorization.
type Flow struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	AuthURL     string `json:"auth_url"`
	RedirectURI string `json:"redirect_uri"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
	AccountID   string `json:"account_id,omitempty"`
	// DBAccountID is the persisted account row this flow created or bound. It is
	// what the admin API must use to return the account: the token's own
	// chatgpt-account-id is absent on the ChatGPT route, so AccountID alone does
	// not identify the stored row.
	DBAccountID int64     `json:"db_account_id,omitempty"`
	Email       string    `json:"email,omitempty"`
	PlanType    string    `json:"plan_type,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	// CallbackAvailable reports whether the local callback listener accepted the port.
	CallbackAvailable bool `json:"callback_available"`
	// Meta carries caller-supplied context (display name, proxy, target account) to
	// OnComplete.
	Meta map[string]string `json:"-"`

	verifier string
	state    string
	nonce    string
	token    *Token
}

// FlowOptions configures the flow manager.
type FlowOptions struct {
	CallbackHost string
	CallbackPort int
	Timeout      time.Duration
	// DeviceID supplies the stable installation UUID used for
	// ext_agent_host_id in the ChatGPT flow.
	DeviceID func() string
	// OnComplete receives a successfully authorized token so it can be persisted.
	// An error marks the flow failed: a sign-in must never be reported as
	// successful when the credential was not actually stored.
	OnComplete func(flow *Flow, token *Token) error
	Logger     *slog.Logger
}

const loginFlowExpiredMessage = "login flow expired"

// WrapCompletion installs persistence middleware before any flows are started.
// The callback runs for both browser callbacks and manually submitted redirects.
func (m *FlowManager) WrapCompletion(wrap func(func(*Flow, *Token) error) func(*Flow, *Token) error) {
	m.opts.OnComplete = wrap(m.opts.OnComplete)
}

// SetHTTPClientFactory binds per-flow egress before the manager starts serving.
// The shared OAuth client supplies configuration, not the flow's transport.
func (m *FlowManager) SetHTTPClientFactory(factory func(string) (*http.Client, error)) {
	m.httpClientFactory = factory
}

// FlowManager tracks pending OAuth flows and owns the localhost callback listener.
type FlowManager struct {
	client *Client
	opts   FlowOptions
	// Configured before serving flows; resolves both explicit direct and proxied transports.
	httpClientFactory func(string) (*http.Client, error)

	mu     sync.Mutex
	flows  map[string]*Flow
	server *http.Server
	ln     net.Listener

	cleanupStop   chan struct{}
	cleanupWG     sync.WaitGroup
	closeOnce     sync.Once
	closed        bool
	closeErr      error
	activeCancels map[string]context.CancelFunc
}

// NewFlowManager builds a flow manager.
func NewFlowManager(client *Client, opts FlowOptions) *FlowManager {
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Minute
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &FlowManager{client: client, opts: opts, flows: map[string]*Flow{}, cleanupStop: make(chan struct{})}
}

// StartFlow creates a pending flow of the given kind.
//
// The local callback listener is attempted first so that a same-machine browser can
// finish the flow automatically; if the port is unavailable (or the gateway runs
// remotely) the user pastes the redirect URL into the web UI instead.
func (m *FlowManager) StartFlow(ctx context.Context, kind string, meta map[string]string) (*Flow, error) {
	if kind == "" {
		kind = FlowKindChatGPT
	}
	if kind != FlowKindChatGPT && kind != FlowKindCodex {
		return nil, fmt.Errorf("oauth: unknown flow kind %q", kind)
	}

	flow, err := m.buildFlow(kind, meta)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("oauth: flow manager is closed")
	}
	m.flows[flow.ID] = flow
	m.mu.Unlock()

	if err := m.ensureCallbackServer(); err == nil {
		m.mu.Lock()
		if current, ok := m.flows[flow.ID]; ok && current == flow {
			current.CallbackAvailable = true
		}
		snapshot := snapshotFlowLocked(flow)
		m.mu.Unlock()
		m.scheduleCleanup(flow.ID)
		return snapshot, nil
	} else {
		m.opts.Logger.Debug("oauth callback listener unavailable; manual paste required", "error", err)
	}

	m.mu.Lock()
	snapshot := snapshotFlowLocked(flow)
	m.mu.Unlock()
	m.scheduleCleanup(flow.ID)
	return snapshot, nil
}

// buildFlow prepares the PKCE material and authorization URL for a kind.
func (m *FlowManager) buildFlow(kind string, meta map[string]string) (*Flow, error) {
	pkce, err := GeneratePKCE()
	if err != nil {
		return nil, err
	}

	flow := &Flow{
		ID:        uuid.NewString(),
		Kind:      kind,
		Status:    FlowPending,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(m.opts.Timeout),
		Meta:      meta,
		verifier:  pkce.Verifier,
	}

	switch kind {
	case FlowKindChatGPT:
		state, err := GenerateRandomValue()
		if err != nil {
			return nil, err
		}
		nonce, err := GenerateRandomValue()
		if err != nil {
			return nil, err
		}
		deviceID := ""
		if m.opts.DeviceID != nil {
			deviceID = m.opts.DeviceID()
		}
		hostID, err := AgentHostID(deviceID)
		if err != nil {
			return nil, err
		}
		// The registered redirect is fixed at 127.0.0.1:1455 regardless of the
		// listener host, exactly as Pi does: a non-default host can only be finished
		// by pasting the redirect URL.
		redirect := ChatGPTRedirectURI("", 0)
		flow.RedirectURI = redirect
		flow.state = state
		flow.nonce = nonce
		flow.AuthURL = BuildChatGPTAuthorizeURL(pkce.Challenge, state, nonce, hostID, redirect)
	case FlowKindCodex:
		state, err := GenerateState()
		if err != nil {
			return nil, err
		}
		flow.RedirectURI = RedirectURI
		flow.state = state
		flow.AuthURL = BuildAuthorizeURL(pkce.Challenge, state, m.client.Originator())
	}
	return flow, nil
}

// Get returns a flow snapshot.
func (m *FlowManager) Get(id string) *Flow {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.flows[id]
	if !ok {
		return nil
	}
	return snapshotFlowLocked(f)
}

// snapshotFlowLocked copies the externally visible state while m.mu is held.
// Meta is copied too, so callers never retain mutable state owned by the manager.
func snapshotFlowLocked(f *Flow) *Flow {
	if f == nil {
		return nil
	}
	snapshot := *f
	if f.Meta != nil {
		snapshot.Meta = make(map[string]string, len(f.Meta))
		for key, value := range f.Meta {
			snapshot.Meta[key] = value
		}
	}
	return &snapshot
}

// SubmitInput completes a pending flow from a pasted authorization code or redirect
// URL.
func (m *FlowManager) SubmitInput(ctx context.Context, id, input string) (*Flow, error) {
	m.mu.Lock()
	flow, ok := m.flows[id]
	if !ok {
		m.mu.Unlock()
		return nil, errors.New("unknown or expired login flow")
	}
	if flow.Status == FlowCompleted || flow.Status == FlowFailed || flow.Status == FlowExchanging {
		snapshot := snapshotFlowLocked(flow)
		m.mu.Unlock()
		return snapshot, fmt.Errorf("login flow is already %s", snapshot.Status)
	}
	if time.Now().After(flow.ExpiresAt) {
		flow.Status = FlowFailed
		flow.Error = loginFlowExpiredMessage
		snapshot := snapshotFlowLocked(flow)
		m.mu.Unlock()
		return snapshot, errors.New(loginFlowExpiredMessage)
	}
	// Keep the internal pointer for completeFlow, but parse against a snapshot so
	// no caller can observe or mutate the live flow.
	parseFlow := snapshotFlowLocked(flow)
	m.mu.Unlock()

	code, clientID, err := m.parseInput(parseFlow, input)
	if err != nil {
		return nil, err
	}
	m.completeFlow(ctx, flow, code, clientID)
	return m.Get(id), nil
}

// parseInput extracts the authorization code (and, for ChatGPT, the issued client
// id) from pasted input.
func (m *FlowManager) parseInput(flow *Flow, input string) (code, clientID string, err error) {
	if flow.Kind == FlowKindChatGPT {
		// The ChatGPT flow requires the full redirect URL so the issued client id and
		// state can be validated.
		return ParseChatGPTCallback(input, flow.state, flow.RedirectURI)
	}
	code, state := ParseAuthorizationInput(input)
	if code == "" {
		return "", "", errors.New("could not find an authorization code in the input")
	}
	if state != "" && state != flow.state {
		return "", "", errors.New("state mismatch: the pasted redirect URL does not belong to this login attempt")
	}
	return code, "", nil
}

func (m *FlowManager) completeFlow(ctx context.Context, flow *Flow, code, clientID string) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m.mu.Lock()
	current, ok := m.flows[flow.ID]
	if m.closed || !ok || current != flow || current.Status != FlowPending {
		m.mu.Unlock()
		return
	}
	if time.Now().After(current.ExpiresAt) {
		current.Status = FlowFailed
		current.Error = loginFlowExpiredMessage
		m.mu.Unlock()
		m.maybeStopCallbackServer()
		return
	}
	current.Status = FlowExchanging
	if m.activeCancels == nil {
		m.activeCancels = make(map[string]context.CancelFunc)
	}
	m.activeCancels[flow.ID] = cancel
	m.cleanupWG.Add(1)
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.activeCancels, flow.ID)
		m.mu.Unlock()
		m.cleanupWG.Done()
	}()

	var (
		tok *Token
		err error
	)
	client := m.client
	if client == nil {
		err = errors.New("oauth: flow client is unavailable")
	} else if m.httpClientFactory != nil {
		var httpClient *http.Client
		httpClient, err = m.httpClientFactory(flow.Meta["proxy_url"])
		if err == nil && httpClient == nil {
			err = errors.New("oauth: flow transport is unavailable")
		}
		if err == nil {
			client = client.WithHTTPClient(httpClient)
		}
	} else if flow.Meta["proxy_url"] != "" {
		err = errors.New("oauth: proxy transport factory is not configured")
	}
	if err == nil && flow.Kind == FlowKindChatGPT {
		tok, err = client.ExchangeChatGPTCode(ctx, code, flow.verifier, clientID, flow.RedirectURI)
		if err == nil {
			// Best effort: only enforced when the provider echoes the nonce.
			err = VerifyIDTokenNonce(tok.IDToken, flow.nonce)
		}
	} else if err == nil {
		tok, err = client.ExchangeCode(ctx, code, flow.verifier, flow.RedirectURI)
	}
	m.finish(flow, tok, err)
}

func (m *FlowManager) finish(flow *Flow, tok *Token, err error) {
	if err != nil {
		m.mu.Lock()
		if current, ok := m.flows[flow.ID]; ok && current == flow && current.Status == FlowExchanging {
			current.Status = FlowFailed
			current.Error = err.Error()
		}
		m.mu.Unlock()
		m.maybeStopCallbackServer()
		return
	}
	if tok == nil {
		m.finish(flow, nil, errors.New("oauth: token exchange returned no token"))
		return
	}

	onComplete := m.opts.OnComplete
	m.mu.Lock()
	current, ok := m.flows[flow.ID]
	if !ok || current != flow || current.Status != FlowExchanging {
		m.mu.Unlock()
		return
	}
	current.AccountID = tok.AccountID
	current.Email = tok.Email
	current.PlanType = tok.PlanType
	current.token = tok
	callbackFlow := snapshotFlowLocked(current)
	m.mu.Unlock()

	// Persisting can fail (a rejected binding, a store write error). The flow must
	// reflect that instead of telling the UI the sign-in succeeded. The callback
	// receives a snapshot, never the live flow.
	if onComplete != nil {
		if persistErr := onComplete(callbackFlow, tok); persistErr != nil {
			m.mu.Lock()
			if current, ok := m.flows[flow.ID]; ok && current == flow && current.Status == FlowExchanging {
				current.Status = FlowFailed
				current.Error = persistErr.Error()
			}
			m.mu.Unlock()
			m.opts.Logger.Error("oauth: could not persist the authorized credential", "kind", flow.Kind, "error", persistErr)
			m.maybeStopCallbackServer()
			return
		}
	}

	m.mu.Lock()
	if current, ok := m.flows[flow.ID]; ok && current == flow && current.Status == FlowExchanging {
		current.DBAccountID = callbackFlow.DBAccountID
		current.Status = FlowCompleted
	}
	m.mu.Unlock()
	m.maybeStopCallbackServer()
}

// failByState marks the pending flow matching an OAuth state as failed so its
// resources (notably the callback listener) are released.
func (m *FlowManager) failByState(state, reason string) {
	if state == "" {
		return
	}
	m.mu.Lock()
	var target *Flow
	for _, f := range m.flows {
		if f.state == state && (f.Status == FlowPending || f.Status == FlowExchanging) {
			target = f
			break
		}
	}
	if target != nil {
		target.Status = FlowFailed
		target.Error = reason
	}
	m.mu.Unlock()
	if target != nil {
		m.maybeStopCallbackServer()
	}
}

// Close stops the callback listener and waits for flow cleanup workers. Tests and
// embedders should call it before closing resources used by OAuth completion.
func (m *FlowManager) Close(ctx context.Context) error {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		close(m.cleanupStop)
		for _, cancel := range m.activeCancels {
			cancel()
		}
		m.failActiveFlowsLocked()
		server, listener := m.server, m.ln
		m.server, m.ln = nil, nil
		m.mu.Unlock()
		if server != nil {
			m.closeErr = server.Shutdown(ctx)
			if m.closeErr != nil {
				m.closeErr = errors.Join(m.closeErr, server.Close())
			}
		}
		if listener != nil {
			if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				m.closeErr = errors.Join(m.closeErr, err)
			}
		}
		m.cleanupWG.Wait()
	})
	return m.closeErr
}

func (m *FlowManager) scheduleCleanup(flowID string) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.cleanupWG.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.cleanupWG.Done()
		flow := m.Get(flowID)
		if flow == nil {
			return
		}
		timer := time.NewTimer(time.Until(flow.ExpiresAt) + time.Minute)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-m.cleanupStop:
			return
		}
		m.mu.Lock()
		if f, ok := m.flows[flowID]; ok {
			if f.Status == FlowPending || f.Status == FlowExchanging {
				f.Status = FlowFailed
				f.Error = loginFlowExpiredMessage
			}
			delete(m.flows, flowID)
		}
		m.mu.Unlock()
		// The expired flow no longer counts as pending, so a listener that only
		// existed for it can be released.
		m.maybeStopCallbackServer()
	}()
}

// ---- localhost callback listener ----

func (m *FlowManager) ensureCallbackServer() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("oauth: flow manager is closed")
	}
	if m.ln != nil {
		return nil
	}

	addr := fmt.Sprintf("%s:%d", m.opts.CallbackHost, m.opts.CallbackPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("oauth: listen %s: %w", addr, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(ChatGPTCallbackPath, m.handleCallback)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	m.ln = ln
	m.server = srv
	m.cleanupWG.Add(1)
	go func() {
		defer m.cleanupWG.Done()
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			m.opts.Logger.Warn("oauth callback server stopped", "error", err)
		}
	}()
	m.opts.Logger.Info("oauth callback listener started", "addr", addr)
	return nil
}

func (m *FlowManager) maybeStopCallbackServer() {
	m.mu.Lock()
	pending := 0
	for _, f := range m.flows {
		if f.Status == FlowPending || f.Status == FlowExchanging {
			pending++
		}
	}
	srv := m.server
	ln := m.ln
	if pending > 0 || srv == nil {
		m.mu.Unlock()
		return
	}
	m.server, m.ln = nil, nil
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	if ln != nil {
		_ = ln.Close()
	}
	m.opts.Logger.Info("oauth callback listener stopped")
}

func (m *FlowManager) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	code := q.Get("code")
	state := q.Get("state")
	errParam := q.Get("error")

	if errParam != "" {
		m.failByState(state, "authorization was refused by the provider: "+errParam+" "+q.Get("error_description"))
		http.Error(w, "Authorization failed: "+errParam+" "+q.Get("error_description"), http.StatusBadRequest)
		return
	}
	if code == "" {
		http.Error(w, "Missing authorization code", http.StatusBadRequest)
		return
	}

	var target *Flow
	m.mu.Lock()
	for _, f := range m.flows {
		if f.state == state && (f.Status == FlowPending || f.Status == FlowExchanging) {
			target = f
			break
		}
	}
	m.mu.Unlock()

	if target == nil {
		http.Error(w, "No pending login attempt matches this callback.", http.StatusBadRequest)
		return
	}

	// Respond before the exchange so the browser is not left waiting on the network hop.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(callbackHTML))

	clientID := ""
	if target.Kind == FlowKindChatGPT {
		clientID = q.Get("client_id")
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.cleanupWG.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.cleanupWG.Done()
		m.completeFlow(context.Background(), target, code, clientID)
	}()
}

const callbackHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Authorization received</title>
<style>
* { box-sizing: border-box; }
body { margin: 0; padding: 24px; min-height: 100vh; display: flex; align-items: center; justify-content: center; background: #18181b; color: #fafafa; font-family: system-ui, -apple-system, sans-serif; }
main { max-width: 480px; text-align: center; }
.logo { width: 96px; height: 96px; margin-bottom: 24px; color: #a1a1aa; }
h1 { font-size: 24px; font-weight: 600; margin: 0 0 16px; }
p { color: #a1a1aa; line-height: 1.6; margin: 0; }
</style></head><body><main>
<svg class="logo" viewBox="0 0 100 100" fill="currentColor" role="img" aria-label="Pi"><path d="M 15 15 L 85 15 L 85 25 L 75 25 L 75 85 L 55 85 L 55 75 L 65 75 L 65 25 L 35 25 L 35 85 L 25 85 L 25 25 L 15 25 Z"/></svg>
<h1>Authorization received</h1><p>You can close this window and return to Pi.</p>
</main></body></html>`

func (m *FlowManager) failActiveFlowsLocked() {
	for _, flow := range m.flows {
		if flow.Status == FlowPending || flow.Status == FlowExchanging {
			flow.Status = FlowFailed
			flow.Error = "oauth flow manager closed"
		}
	}
}
