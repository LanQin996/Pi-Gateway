package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"pi-gateway/internal/config"
	"pi-gateway/internal/egress"
)

const (
	codexLatestReleaseURL      = "https://api.github.com/repos/openai/codex/releases/latest"
	codexVersionCacheTTL       = time.Hour
	codexVersionRetryDelay     = time.Minute
	codexVersionRequestTimeout = 5 * time.Second
	maxCodexReleaseBytes       = 256 << 10
	codexCatalogOriginator     = "codex_cli_rs"
)

// codexVersionCache holds public release metadata, not account credentials. One
// in-flight lookup and one cached version bound both memory and network work.
// It is deliberately separate from the account/credential refresh locks.
type codexVersionCache struct {
	mu        sync.Mutex
	version   string
	nextCheck time.Time
	inflight  chan struct{}
}

func (m *Manager) resolveCodexClientVersion(ctx context.Context, configured, proxyURL string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if configured != "" && configured != config.AutoCodexClientVersion {
		return configured, nil
	}
	cache := &m.codexVersions
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		cache.mu.Lock()
		if cache.version != "" && m.nowTime().Before(cache.nextCheck) {
			version := cache.version
			cache.mu.Unlock()
			return version, nil
		}
		if pending := cache.inflight; pending != nil {
			cache.mu.Unlock()
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-pending:
				continue
			}
		}
		pending := make(chan struct{})
		cache.inflight = pending
		cache.mu.Unlock()

		return m.completeCodexVersionLookup(ctx, proxyURL, cache, pending)
	}
}

func (m *Manager) fetchLatestCodexClientVersion(ctx context.Context, proxyURL string) (string, error) {
	if m.factory == nil {
		return "", errors.New("release HTTP client is unavailable")
	}
	client, err := m.factory.HTTPClient(proxyURL)
	if err != nil {
		return "", errors.New("release proxy configuration is invalid")
	}
	requestCtx, cancel := context.WithTimeout(ctx, codexVersionRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, codexLatestReleaseURL, nil)
	if err != nil {
		return "", errors.New("release request could not be constructed")
	}
	// Public GitHub metadata must never inherit the account or incoming headers.
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "pi-gateway-codex-version")
	bounded := *client
	bounded.Jar = nil
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := bounded.Do(request)
	if err != nil {
		if requestCtx.Err() != nil {
			return "", errors.New("release request timed out or was canceled")
		}
		return "", errors.New("release connection failed")
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return "", fmt.Errorf("release endpoint returned HTTP %d", response.StatusCode)
	}
	body, err := egress.DecodeBody(response)
	if err != nil {
		response.Body.Close()
		return "", errors.New("release response could not be decompressed")
	}
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, maxCodexReleaseBytes+1))
	if err != nil {
		return "", errors.New("release response could not be read")
	}
	if len(raw) > maxCodexReleaseBytes {
		return "", errors.New("release response exceeds size limit")
	}
	return parseCodexReleaseVersion(raw)
}

func parseCodexReleaseVersion(raw []byte) (string, error) {
	var release struct {
		TagName    string `json:"tag_name"`
		Name       string `json:"name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.Unmarshal(raw, &release); err != nil {
		return "", errors.New("release response is invalid JSON")
	}
	if release.Draft || release.Prerelease {
		return "", errors.New("release is not stable")
	}
	rawVersion := strings.TrimSpace(release.TagName)
	if rawVersion == "" {
		rawVersion = strings.TrimSpace(release.Name)
	}
	// The official tag is authoritative: never turn an alpha tag into a stable
	// release by falling back to a different display name.
	version := strings.TrimPrefix(strings.TrimPrefix(rawVersion, "rust-"), "v")
	if _, ok := codexVersionParts(version); !ok {
		return "", errors.New("release does not contain a valid stable version")
	}
	return version, nil
}

func codexVersionParts(version string) ([3]uint64, bool) {
	var result [3]uint64
	parts := strings.Split(version, ".")
	if len(version) > 64 || len(parts) != len(result) {
		return result, false
	}
	for i, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return result, false
		}
		for _, ch := range part {
			if ch < '0' || ch > '9' {
				return result, false
			}
		}
		n, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return result, false
		}
		result[i] = n
	}
	return result, true
}

// Both arguments are validated release versions or the built-in fallback.
func compareCodexVersions(a, b string) int {
	left, _ := codexVersionParts(a)
	right, _ := codexVersionParts(b)
	for i := range left {
		if left[i] < right[i] {
			return -1
		}
		if left[i] > right[i] {
			return 1
		}
	}
	return 0
}

func (m *Manager) completeCodexVersionLookup(ctx context.Context, proxyURL string, cache *codexVersionCache, pending chan struct{}) (string, error) {
	fetched, fetchErr := m.fetchLatestCodexClientVersion(ctx, proxyURL)
	cache.mu.Lock()
	// A caller's cancellation must not install a process-wide failure cache.
	// Waiters with live contexts can start a fresh attempt after this one ends.
	if err := ctx.Err(); err != nil {
		cache.inflight = nil
		close(pending)
		cache.mu.Unlock()
		return "", err
	}
	if cache.version == "" {
		cache.version = config.DefaultCodexClientVersion
	}
	delay := codexVersionCacheTTL
	if fetchErr != nil {
		delay = codexVersionRetryDelay
	} else if compareCodexVersions(fetched, cache.version) > 0 {
		cache.version = fetched
	}
	cache.nextCheck = m.nowTime().Add(delay)
	version := cache.version
	cache.inflight = nil
	close(pending)
	cache.mu.Unlock()
	if fetchErr != nil && m.logger != nil {
		// fetchErr is classified locally; never log transport errors, URLs,
		// proxy passwords or the release response body.
		m.logger.Warn("Codex client version lookup failed; using fallback", "version", version, "reason", fetchErr.Error())
	}
	return version, nil
}
