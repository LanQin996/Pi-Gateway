package upstream

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"pi-gateway/internal/session"
)

// ErrContinuationUnavailable requires a new chain with complete input, never a retry.
var ErrContinuationUnavailable = errors.New("upstream: continuation unavailable; send complete input without previous_response_id")

type ContinuationError struct{ Cause error }

func (e *ContinuationError) Error() string        { return ErrContinuationUnavailable.Error() }
func (e *ContinuationError) Unwrap() error        { return e.Cause }
func (e *ContinuationError) Is(target error) bool { return target == ErrContinuationUnavailable }

func newConnectionID() string {
	var id [24]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic("upstream: secure random unavailable")
	}
	return hex.EncodeToString(id[:])
}

// identityHash contains only a fingerprint, never the actual credentials. The
// session headers intentionally do not participate: a downstream reconnect may
// carry a different hint while still addressing the same authenticated socket.
func (c *Client) identityHash(req *Request) string {
	identity := []string{c.cfg.WSURL, req.ProxyURL}
	names := []string{}
	for name := range req.WSHeaders {
		if strings.EqualFold(name, "session-id") || strings.EqualFold(name, "x-client-request-id") {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		identity = append(identity, strings.ToLower(name))
		identity = append(identity, req.WSHeaders.Values(name)...)
	}
	raw, _ := json.Marshal(identity)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (c *Client) requestPoolKey(req *Request) string {
	scope := req.PoolSessionID
	// Missing ownership or scope cannot become a globally shared anonymous lane.
	if req.ClientKeyID <= 0 || scope == "" {
		scope = newConnectionID()
	}
	raw, _ := json.Marshal([]any{req.ClientKeyID, req.AccountID, scope, c.identityHash(req)})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func requestPreviousID(req *Request) (string, error) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return "", err
	}
	id := req.PreviousResponseID
	if raw, present := body["previous_response_id"]; present && string(raw) != "null" {
		var wire string
		if json.Unmarshal(raw, &wire) != nil || strings.TrimSpace(wire) == "" || (id != "" && id != wire) {
			return "", &ContinuationError{Cause: session.ErrInvalid}
		}
		id = wire
	}
	// Do not accept metadata that is absent from the actual wire request.
	if id != "" {
		var wire string
		if json.Unmarshal(body["previous_response_id"], &wire) != nil || wire != id {
			return "", &ContinuationError{Cause: session.ErrInvalid}
		}
	}
	return id, nil
}

// ResolveContinuation first uses trusted metadata held by a live local socket.
// Shared records can locate a candidate, but never prove socket liveness.
func (c *Client) ResolveContinuation(ctx context.Context, keyID int64, responseID string) (*session.Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, &ContinuationError{Cause: err}
	}
	if keyID <= 0 || responseID == "" {
		return nil, &ContinuationError{Cause: session.ErrInvalid}
	}
	if record := c.pool.resolve(keyID, responseID); record != nil {
		if !c.validRecord(record, keyID, responseID) {
			return nil, &ContinuationError{Cause: session.ErrInvalid}
		}
		return record, nil
	}
	if c.cfg.Sessions == nil {
		return nil, &ContinuationError{Cause: session.ErrMiss}
	}
	record, err := c.cfg.Sessions.Resolve(ctx, keyID, responseID)
	if err != nil {
		return nil, &ContinuationError{Cause: err}
	}
	if !c.validRecord(record, keyID, responseID) {
		return nil, &ContinuationError{Cause: session.ErrInvalid}
	}
	// A different instance, vanished socket, or old parent is not a resumable chain.
	if !c.pool.matches(record) {
		return nil, &ContinuationError{Cause: session.ErrMiss}
	}
	return record, nil
}

func (c *Client) validRecord(r *session.Record, keyID int64, responseID string) bool {
	return r != nil && r.KeyID == keyID && keyID > 0 && r.AccountID > 0 && r.ResponseID == responseID &&
		r.Scope == session.ScopeConnectionLocal && r.SchemaVersion == 1 && r.Version == 1 && r.Provider == "openai-codex" && r.InstanceID == c.instanceID &&
		r.ConnectionID != "" && r.SessionID != "" && r.IdentityHash != "" && !r.CreatedAt.IsZero() &&
		r.CreatedAt.Before(time.Now().Add(time.Minute)) && r.ExpiresAt.After(time.Now()) && r.CodexBridge.Valid()
}

func (c *Client) completedRecord(req *Request, conn *wsConn, key, responseID string) *session.Record {
	if key == "" || req.ClientKeyID <= 0 || req.AccountID <= 0 || responseID == "" || !req.CodexBridge.Valid() {
		return nil
	}
	now := time.Now()
	return &session.Record{KeyID: req.ClientKeyID, AccountID: req.AccountID, ResponseID: responseID,
		SessionID: key, InstanceID: c.instanceID, ConnectionID: conn.id, IdentityHash: c.identityHash(req), CodexBridge: req.CodexBridge,
		Provider: "openai-codex", Scope: session.ScopeConnectionLocal, SchemaVersion: 1, Version: 1, CreatedAt: now, ExpiresAt: now.Add(c.cfg.SessionTTL)}
}

// One bounded best-effort write before delivering the successful terminal event.
// Immutable response records ensure even a late write cannot advance a socket.
func (c *Client) commitShared(ctx context.Context, record *session.Record) {
	if c.cfg.Sessions == nil || record == nil || ctx.Err() != nil {
		return
	}
	select {
	case c.commits <- struct{}{}:
	default:
		return
	}
	commitCtx, cancel := context.WithTimeout(ctx, c.cfg.SessionCommitTimeout)
	defer cancel()
	done := make(chan error, 1)
	value := *record
	go func() {
		defer func() { <-c.commits }()
		done <- c.cfg.Sessions.RecordCompleted(commitCtx, value)
	}()
	select {
	case <-commitCtx.Done():
		c.cfg.Logger.Warn("continuation shared commit timed out")
	case err := <-done:
		if err != nil {
			c.cfg.Logger.Warn("continuation shared commit unavailable")
		}
	}
}

func successfulTerminal(event *Event) bool {
	if event.Type != EventResponseCompleted && event.Type != EventResponseDone {
		return false
	}
	response, ok := event.Data["response"].(map[string]any)
	if !ok {
		return false
	}
	status, _ := response["status"].(string)
	return status == "" || status == "completed"
}

func continuationMismatch() error {
	return &ContinuationError{Cause: fmt.Errorf("local socket state no longer matches")}
}
