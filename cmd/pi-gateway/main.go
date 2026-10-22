// Command pi-gateway is a multi-account gateway that proxies the OpenAI
// "responses" protocol to api.openai.com the way Pi's openai-chatgpt provider
// does, keeping an optional Codex credential for quota reporting only.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"pi-gateway/internal/accounts"
	"pi-gateway/internal/admin"
	"pi-gateway/internal/api"
	"pi-gateway/internal/config"
	"pi-gateway/internal/egress"
	"pi-gateway/internal/logging"
	"pi-gateway/internal/middleware"
	"pi-gateway/internal/oauth"
	"pi-gateway/internal/quota"
	"pi-gateway/internal/rulesruntime"
	"pi-gateway/internal/session"
	"pi-gateway/internal/settings"
	"pi-gateway/internal/store"
	"pi-gateway/internal/upstream"
	"pi-gateway/internal/webui"
)

// version is overridden at build time with -ldflags.
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", "config.yaml", "path to the YAML configuration file")
		listenHost  = flag.String("host", "", "override server.host")
		listenPort  = flag.Int("port", 0, "override server.port")
		showVersion = flag.Bool("version", false, "print the version and exit")
		healthcheck = flag.Bool("healthcheck", false, "check local /healthz and exit without opening the database")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("pi-gateway", version)
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *listenHost != "" {
		cfg.Server.Host = *listenHost
	}
	if *listenPort != 0 {
		cfg.Server.Port = *listenPort
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if *healthcheck {
		return checkHealth(context.Background(), cfg.Server.Host, cfg.Server.Port)
	}

	return runGateway(cfg)
}

func runGateway(cfg *config.Config) error {
	logger, closeLog, err := logging.Setup(cfg.Logging.Level, cfg.Logging.File)
	if err != nil {
		return err
	}
	defer func() { _ = closeLog() }()

	logger.Info("starting pi-gateway", "version", version, "listen", cfg.Addr(), "upstream", cfg.Upstream.BaseURL)

	st, err := store.OpenOptions(store.Options{
		Driver: cfg.Data.Driver, DSN: cfg.Data.DSN, Path: cfg.Data.Database,
		MaxOpenConns: cfg.Data.MaxOpenConns, MaxIdleConns: cfg.Data.MaxIdleConns,
		ConnMaxLifetime: time.Duration(cfg.Data.ConnMaxLifetimeSeconds) * time.Second,
	})
	if err != nil {
		return err
	}
	defer st.Close()

	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	identityCtx, identityCancel := context.WithTimeout(rootCtx, 10*time.Second)
	deploymentID, err := st.DeploymentID(identityCtx)
	identityCancel()
	if err != nil {
		return err
	}
	sessions, err := newSessionStore(cfg, deploymentID)
	if err != nil {
		return err
	}
	defer sessions.Close()

	settingsHolder := settings.New(st, cfg)
	if err := settingsHolder.Load(rootCtx); err != nil {
		return err
	}

	if err := initializeRules(rootCtx, st, logger); err != nil {
		return err
	}

	factory := egress.NewFactory(egress.Options{
		ConnectTimeout: time.Duration(cfg.Upstream.ConnectTimeoutSeconds) * time.Second,
		IdleTimeout:    time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second,
	})

	oauthClient := oauth.NewClient(nil)
	oauthClient.SetOriginator(cfg.Upstream.Originator)

	accountsMgr := accounts.New(st, oauthClient, factory, accounts.Options{
		RefreshMargin:    time.Duration(cfg.Accounts.RefreshMarginSeconds) * time.Second,
		MaxPerAccount:    cfg.Accounts.MaxConcurrentPerAccount,
		Logger:           logger,
		Affinity:         sessions,
		AffinityIdentity: session.Fingerprint("gateway-upstream", cfg.UpstreamSSEURL()+"\x00"+cfg.UpstreamWSURL()+"\x00"+cfg.Upstream.Originator),
		AffinityTTL:      time.Duration(cfg.Redis.SessionTTLSeconds) * time.Second,
		AffinityTimeout:  time.Duration(cfg.Redis.CommitTimeoutMS) * time.Millisecond,
	})
	accountsMgr.SetSettings(settingsHolder.Get())

	// The ChatGPT flow identifies this installation with a stable UUID
	// (ext_agent_host_id). Persist it so a re-login keeps the same host identity.
	deviceUUID := resolveDeviceUUID(rootCtx, st, logger)

	// OAuth completions persist the credential, whether the flow finished through
	// the localhost callback or a pasted redirect URL. A ChatGPT flow creates (or
	// refreshes) the account; a Codex flow is attached to an existing account and is
	// used for quota reads and Codex catalog synchronization, not generation.
	flows := oauth.NewFlowManager(oauthClient, oauth.FlowOptions{
		CallbackHost: cfg.OAuth.CallbackHost,
		CallbackPort: cfg.OAuth.CallbackPort,
		Timeout:      time.Duration(cfg.OAuth.TimeoutSeconds) * time.Second,
		DeviceID:     func() string { return deviceUUID },
		Logger:       logger,
		OnComplete:   (&oauthCompletion{st: st, accounts: accountsMgr, logger: logger}).complete,
	})

	compactionKey, err := st.CompactionKey(rootCtx)
	if err != nil {
		return err
	}
	upstreamClient := upstream.New(upstream.Config{
		CompactionKey:         compactionKey,
		SSEURL:                cfg.UpstreamSSEURL(),
		WSURL:                 cfg.UpstreamWSURL(),
		Originator:            cfg.Upstream.Originator,
		UserAgent:             cfg.Upstream.UserAgent,
		Zstd:                  cfg.Upstream.SSEZstd,
		ConnectTimeout:        time.Duration(cfg.Upstream.ConnectTimeoutSeconds) * time.Second,
		IdleTimeout:           time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second,
		Pool:                  cfg.Upstream.WebsocketPool,
		Sessions:              sessions,
		SessionCommitTimeout:  time.Duration(cfg.Redis.CommitTimeoutMS) * time.Millisecond,
		SessionTTL:            time.Duration(cfg.Redis.SessionTTLSeconds) * time.Second,
		MaxBaselineBytes:      cfg.Upstream.WebsocketMaxBaselineBytes,
		MaxTotalBaselineBytes: cfg.Upstream.WebsocketMaxTotalBaselineBytes,
		MaxPoolConnections:    cfg.Upstream.WebsocketMaxPoolConnections,
		Logger:                logger,
	}, factory)
	defer upstreamClient.Close()

	// Quota reporting uses the Codex/ChatGPT backend, never the model API base
	// (cfg.Upstream.BaseURL may be https://api.openai.com/v1).
	accountsMgr.SetQuotaClient(quota.New(quota.Config{
		BaseURL:         quota.CodexBaseURL,
		OfficialBaseURL: quota.CodexBaseURL,
		Originator:      cfg.Upstream.Originator,
		UserAgentFunc:   quotaUserAgent(settingsHolder, cfg.Upstream.UserAgent),
		Timeout:         20 * time.Second,
	}, factory, logger))

	accountsMgr.StartRefresher(rootCtx,
		time.Duration(cfg.Accounts.RefreshIntervalSeconds)*time.Second,
		cfg.Accounts.RefreshConcurrency,
	)
	accountsMgr.StartModelCatalogRefresher(rootCtx,
		cfg.Models.RefreshIntervalSeconds,
		cfg.Accounts.RefreshConcurrency,
		cfg,
		func() string { return settingsHolder.Get().UserAgent },
	)
	accountsMgr.StartQuotaRefresher(rootCtx,
		cfg.Accounts.QuotaRefreshSeconds,
		cfg.Accounts.RefreshConcurrency,
		2*time.Second,
	)
	accountsMgr.StartOfficialUsageRefresher(rootCtx)

	dataPlane := api.New(api.Options{
		Config:         cfg,
		Store:          st,
		Accounts:       accountsMgr,
		Upstream:       upstreamClient,
		Settings:       settingsHolder,
		Logger:         logger,
		CatalogCache:   sessions,
		CatalogTTL:     time.Duration(cfg.Redis.CatalogTTLSeconds) * time.Second,
		CatalogTimeout: time.Duration(cfg.Redis.CommitTimeoutMS) * time.Millisecond,
	})

	defer dataPlane.Close()

	adminServer := admin.New(admin.Options{
		Config:   cfg,
		Store:    st,
		Accounts: accountsMgr,
		Settings: settingsHolder,
		Flows:    flows,
		Upstream: upstreamClient,
		Factory:  factory,
		Logger:   logger,
	})
	if err := adminServer.BootstrapPassword(rootCtx); err != nil {
		return err
	}

	mux := http.NewServeMux()
	dataPlane.Routes(mux)
	adminServer.Routes(mux, webui.Handler())

	srv := &http.Server{
		Addr:              cfg.Addr(),
		Handler:           withRequestLogging(logger, mux),
		ReadHeaderTimeout: 30 * time.Second,
		// Streaming responses are long lived, so no write timeout is enforced.
		IdleTimeout: 120 * time.Second,
	}

	printStartup(logger, cfg, adminServer.GeneratedPassword(), st)

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	go runUsageMaintenance(rootCtx, st, settingsHolder, logger)

	select {
	case err := <-errCh:
		return err
	case <-rootCtx.Done():
		logger.Info("shutdown signal received")
	}

	// Hijacked WebSockets are not covered by http.Server.Shutdown.
	dataPlane.Close()
	upstreamClient.Close()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("graceful shutdown failed", "error", err)
	}
	return nil
}

// seedMiddlewares installs the default middleware configuration on first run so
// the web UI reflects real state instead of guessing.
func seedMiddlewares(ctx context.Context, st *store.Store, logger *slog.Logger) error {
	existing, err := st.ListMiddlewares(ctx)
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		return nil
	}

	registry := middleware.Registry()

	// The environment context must be dropped to match Pi, and unknown fields must
	// not reach the backend; everything else is opt-in.
	defaults := map[string]struct {
		enabled bool
		order   int
	}{
		"drop_environment_context": {enabled: true, order: 10},
		"drop_fields":              {enabled: true, order: 20},
		"drop_input_items":         {enabled: false, order: 30},
		"drop_tools":               {enabled: false, order: 40},
		"rewrite_model":            {enabled: false, order: 50},
		"set_reasoning":            {enabled: false, order: 60},
		"passthrough_fields":       {enabled: false, order: 70},
		"block_prompt":             {enabled: false, order: 80},
	}

	for name, mw := range registry {
		cfg := mw.DefaultConfig()
		state := defaults[name]
		if err := st.UpsertMiddleware(ctx, &store.MiddlewareRow{
			Name:       name,
			Enabled:    state.enabled,
			OrderIndex: state.order,
			Config:     cfg,
		}); err != nil {
			return err
		}
	}
	logger.Info("seeded default middleware configuration", "count", len(registry))
	return nil
}

// withRequestLogging logs API requests at debug level and recovers from panics.
func withRequestLogging(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tracked := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("panic serving request", "path", r.URL.Path, "panic", fmt.Sprint(rec))
				if !tracked.wrote {
					http.Error(tracked, "internal error", http.StatusInternalServerError)
				}
			}
		}()

		// Health checks and static assets would drown the log.
		if strings.HasPrefix(r.URL.Path, "/api") || strings.HasPrefix(r.URL.Path, "/v1") ||
			strings.Contains(r.URL.Path, "/responses") {
			logger.Debug("request", "method", r.Method, "path", r.URL.Path)
		}
		var wrapped http.ResponseWriter = tracked
		// WebSocket upgrades require http.Hijacker. Keep that optional interface
		// conditional: exposing it when the underlying writer cannot hijack would
		// make handlers attempt an unsupported upgrade.
		if _, ok := w.(http.Hijacker); ok {
			wrapped = &hijackStatusRecorder{statusRecorder: tracked}
		}
		next.ServeHTTP(wrapped, r)
	})
}

// statusRecorder remembers whether the response was started, so the panic handler
// does not write a second header. Optional writer capabilities are forwarded to the
// underlying writer where possible so streaming and fast-path copies keep working.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

// Unwrap lets http.ResponseController reach capabilities of the underlying writer.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// hijackStatusRecorder adds Hijacker only when the underlying writer supports it.
type hijackStatusRecorder struct {
	*statusRecorder
}

func (s *hijackStatusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := s.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}

func (s *statusRecorder) WriteHeader(status int) {
	if s.wrote {
		return
	}
	s.status = status
	s.wrote = true
	s.ResponseWriter.WriteHeader(status)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.WriteHeader(http.StatusOK)
	}
	return s.ResponseWriter.Write(b)
}

// Flush keeps streaming responses working through the wrapper.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusRecorder) ReadFrom(src io.Reader) (int64, error) {
	if rf, ok := s.ResponseWriter.(io.ReaderFrom); ok {
		if !s.wrote {
			s.WriteHeader(http.StatusOK)
		}
		return rf.ReadFrom(src)
	}
	// Do not pass s directly to io.Copy here: s implements io.ReaderFrom, so
	// io.Copy(s, src) would dispatch right back into this method recursively.
	return io.Copy(struct{ io.Writer }{Writer: s}, src)
}

func (s *statusRecorder) Push(target string, opts *http.PushOptions) error {
	if p, ok := s.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}

func printStartup(logger *slog.Logger, cfg *config.Config, generatedPassword string, st *store.Store) {
	base := fmt.Sprintf("http://%s", cfg.Addr())
	logger.Info("admin UI ready", "url", base)
	logger.Info("client endpoints",
		"responses_sse", base+"/v1/responses",
		"responses_ws", "ws"+strings.TrimPrefix(base, "http")+"/v1/responses",
		"pi_base_url_hint", base+"/v1",
	)

	if generatedPassword != "" {
		logger.Warn("generated an admin password; change it after logging in",
			"username", cfg.Admin.Username,
			"password", generatedPassword,
		)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	accounts, err := st.ListAccounts(ctx)
	if err == nil && len(accounts) == 0 {
		logger.Warn("no accounts configured yet; add one from the admin UI to start serving traffic")
	}
}

// resolveDeviceUUID returns the persisted installation UUID used as
// ext_agent_host_id in the Sign in with ChatGPT flow, generating and storing one on
// first use.
func resolveDeviceUUID(ctx context.Context, st *store.Store, logger *slog.Logger) string {
	const key = "oauth_device_uuid"
	if existing, err := st.GetSecret(ctx, key); err == nil && strings.TrimSpace(existing) != "" {
		return existing
	}
	generated := oauth.NewDeviceUUID()
	if err := st.SetSecret(ctx, key, generated); err != nil {
		logger.Warn("could not persist the oauth device uuid; using an ephemeral one", "error", err)
	}
	return generated
}

// runUsageMaintenance reconciles interrupted ledger rows and enforces the usage
// retention window.
//
// It mirrors the reference implementation's periodic pass: rows left "running"
// by a crash become "incomplete", and finished rows older than
// usage_retention_days are deleted in bounded batches so a large table never
// turns into one long transaction.
func runUsageMaintenance(ctx context.Context, st *store.Store, holder *settings.Holder, logger *slog.Logger) {
	if st == nil {
		return
	}
	const (
		staleAfter   = 15 * time.Minute
		batchSize    = 5000
		maxBatches   = 12
		defaultDays  = 31
		hourlyPeriod = time.Hour
	)

	reconcile := func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		n, err := st.ReconcileRunningUsageRecords(rctx, time.Now().Add(-staleAfter).UnixMilli())
		if err != nil {
			logger.Warn("reconciling usage records failed", "error", err)
		} else if n > 0 {
			logger.Info("marked interrupted usage records incomplete", "rows", n)
		}
	}
	prune := func() {
		days := defaultDays
		if holder != nil {
			if rt := holder.Get(); rt != nil && rt.UsageRetentionDays > 0 {
				days = rt.UsageRetentionDays
			}
		}
		cutoff := time.Now().AddDate(0, 0, -days).UnixMilli()
		pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		n, err := st.DeleteUsageRecordsBefore(pctx, cutoff, batchSize, maxBatches)
		if err != nil {
			logger.Warn("pruning usage records failed", "error", err)
		} else if n > 0 {
			logger.Info("pruned usage records", "rows", n, "retention_days", days)
		}
	}

	// Startup pass: the previous process may have died mid-stream.
	reconcile()

	ticker := time.NewTicker(hourlyPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcile()
			prune()
		}
	}
}

type oauthCompletion struct {
	st       *store.Store
	accounts *accounts.Manager
	logger   *slog.Logger
}

func (h *oauthCompletion) complete(flow *oauth.Flow, tok *oauth.Token) error {
	name, proxyURL, accountRef := "", "", ""
	if flow.Meta != nil {
		name = flow.Meta["name"]
		proxyURL = flow.Meta["proxy_url"]
		accountRef = flow.Meta["account_id"]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if flow.Kind == oauth.FlowKindCodex {
		return h.linkCodex(ctx, flow, tok, accountRef)
	}

	if accountRef != "" {
		id, err := strconv.ParseInt(accountRef, 10, 64)
		if err != nil {
			return errors.New("invalid ChatGPT target account")
		}
		acc, err := h.st.GetAccount(ctx, id)
		if err != nil || acc == nil {
			return errors.New("ChatGPT target account not found")
		}
		if err := h.accounts.LinkChatGPTCredential(ctx, acc, tok); err != nil {
			return err
		}
		flow.DBAccountID = acc.ID
		return nil
	}
	acc, err := h.accounts.CreateFromToken(ctx, name, tok, proxyURL)
	if err != nil {
		return fmt.Errorf("persisting the authorized account failed: %w", err)
	}
	flow.DBAccountID = acc.ID
	h.logger.Info("account authorized", "account", acc.Name, "account_id", acc.AccountID, "plan", acc.PlanType)
	return nil
}

func (h *oauthCompletion) linkCodex(ctx context.Context, flow *oauth.Flow, tok *oauth.Token, accountRef string) error {
	if accountRef == "" {
		return errors.New("codex credential finished without a target account")
	}
	id, parseErr := strconv.ParseInt(accountRef, 10, 64)
	if parseErr != nil {
		return fmt.Errorf("codex credential target account id %q is invalid: %w", accountRef, parseErr)
	}
	acc, findErr := h.st.GetAccount(ctx, id)
	if findErr != nil {
		return fmt.Errorf("codex credential target account lookup failed: %w", findErr)
	}
	if acc == nil {
		return fmt.Errorf("codex credential target account %s was not found", accountRef)
	}
	if linkErr := h.accounts.LinkCodexCredential(ctx, acc, tok); linkErr != nil {
		return fmt.Errorf("linking the codex credential failed: %w", linkErr)
	}
	flow.DBAccountID = acc.ID
	h.logger.Info("codex credential linked", "account", acc.Name)
	return nil
}

func initializeRules(ctx context.Context, st *store.Store, logger *slog.Logger) error {
	if err := seedMiddlewares(ctx, st, logger); err != nil {
		return err
	}
	// Publish the legacy middleware configuration exactly once. The data plane
	// only runs the immutable rule snapshot after this succeeds.
	if err := rulesruntime.New(st).Ensure(ctx); err != nil {
		if !errors.Is(err, rulesruntime.ErrMigration) {
			return fmt.Errorf("initialize rules: %w", err)
		}
		// Keep the admin repair path and unchanged legacy chain available. Never
		// turn an unconvertible policy into an empty, permissive new rule set.
		logger.Error("rule migration requires repair; retaining legacy middleware", "error", err)
	}

	return nil
}

func quotaUserAgent(holder *settings.Holder, fallback string) func() string {
	return func() string {
		if rt := holder.Get(); rt != nil && rt.UserAgent != "" {
			return rt.UserAgent
		}
		return fallback
	}
}
