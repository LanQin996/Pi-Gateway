// Package session stores bounded continuation metadata, not request bodies,
// transcripts, credentials, or live socket/lifecycle objects. Provider opaque
// state must be a provider-issued continuation handle, never a copied transcript.
package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"pi-gateway/internal/codexbridge"
)

const (
	MaxRecordBytes             = 64 << 10
	DefaultTTL                 = 4 * time.Hour
	DefaultMaxEntries          = 10000
	DefaultMaxBytes      int64 = 64 << 20
	DefaultCommitTimeout       = 100 * time.Millisecond
)

var (
	ErrMiss        = errors.New("session: miss")
	ErrNotFound    = ErrMiss
	ErrWrongOwner  = errors.New("session: wrong owner")
	ErrInvalid     = errors.New("session: invalid")
	ErrUnavailable = errors.New("session: unavailable")
)

type Status string

const (
	Hit         Status = "hit"
	Miss        Status = "miss"
	WrongOwner  Status = "wrong_owner"
	Invalid     Status = "invalid"
	Unavailable Status = "unavailable"
)

// StatusOf also classifies successful RecordCompleted/Invalidate calls as Hit.
func StatusOf(err error) Status {
	switch {
	case err == nil:
		return Hit
	case errors.Is(err, ErrMiss):
		return Miss
	case errors.Is(err, ErrWrongOwner):
		return WrongOwner
	case errors.Is(err, ErrInvalid):
		return Invalid
	default:
		return Unavailable
	}
}

type Scope string

const (
	ScopePersisted       Scope = "persisted"
	ScopeConnectionLocal Scope = "connection_local"
)

// Record is an allowlist of safe metadata. ResponseID never enters serialized
// storage. SessionID is an opaque internal pool anchor, NOT a raw client session
// header; callers must derive it without embedding client input or credentials.
// IdentityHash binds the provider/credential revision, but contains no credential.
// Version is the expected version: zero means create, or an identical retry;
// updates pass the version returned by Resolve and atomically increment it.
// A connection_local hit is not proof of a live socket: callers must separately
// verify instance, connection generation, and that response is the socket's latest.
type Record struct {
	ResponseID          string              `json:"-"`
	KeyID               int64               `json:"key_id,string"`
	AccountID           int64               `json:"account_id,string"`
	SessionID           string              `json:"session_id"`
	InstanceID          string              `json:"instance_id"`
	ConnectionID        string              `json:"connection_id"`
	IdentityHash        string              `json:"identity_hash"`
	Provider            string              `json:"provider"`
	Scope               Scope               `json:"scope"`
	Version             uint64              `json:"version,string"`
	CreatedAt           time.Time           `json:"created_at"`
	ExpiresAt           time.Time           `json:"expires_at"`
	OpaqueState         []byte              `json:"opaque_state,omitempty"`
	CodexBridge         codexbridge.Profile `json:"codex_bridge,omitempty"`
	ResponseFingerprint string              `json:"response_fingerprint"`
	SessionFingerprint  string              `json:"session_fingerprint"`
	SchemaVersion       int                 `json:"schema_version"`
}

// Invalidation cannot remove a replacement belonging to another socket or version.
type Invalidation struct {
	KeyID        int64
	ResponseID   string
	InstanceID   string
	ConnectionID string
	Version      uint64
}

type Store interface {
	Resolve(context.Context, int64, string) (*Record, error)
	RecordCompleted(context.Context, Record) error
	Invalidate(context.Context, Invalidation) error
}

type Options struct {
	Namespace      string
	TTL            time.Duration
	MaxEntries     int
	MaxBytes       int64
	MaxRecordBytes int
	CommitTimeout  time.Duration
}

func (o Options) normalized() Options {
	if strings.TrimSpace(o.Namespace) == "" {
		o.Namespace = "pi-gateway:session"
	}
	if o.TTL < time.Millisecond {
		o.TTL = DefaultTTL
	}
	if o.MaxEntries <= 0 {
		o.MaxEntries = DefaultMaxEntries
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultMaxBytes
	}
	if o.MaxRecordBytes <= 0 || o.MaxRecordBytes > MaxRecordBytes {
		o.MaxRecordBytes = MaxRecordBytes
	}
	if o.CommitTimeout <= 0 {
		o.CommitTimeout = DefaultCommitTimeout
	}
	return o
}

// Fingerprint domain-separates response/session/cache identifiers. The namespace
// must be configured, never derived from an untrusted identifier.
func Fingerprint(domain, value string) string {
	h := sha256.Sum256([]byte(domain + "\x00" + value))
	return hex.EncodeToString(h[:])
}

func validReference(keyID int64, responseID string) bool {
	return keyID > 0 && responseID != "" && len(responseID) <= 4096
}

func (r Record) valid() bool {
	if r.KeyID <= 0 || r.AccountID <= 0 || r.SessionID == "" || r.Provider == "" || r.IdentityHash == "" {
		return false
	}
	if r.Scope != ScopePersisted && r.Scope != ScopeConnectionLocal {
		return false
	}
	if r.InstanceID == "" || r.ConnectionID == "" {
		return false
	}
	for _, v := range []string{r.SessionID, r.InstanceID, r.ConnectionID, r.IdentityHash, r.Provider} {
		if len(v) > 4096 {
			return false
		}
	}
	return r.Version > 0 && r.SchemaVersion == 1 && r.CodexBridge.Valid() && len(r.OpaqueState) <= MaxRecordBytes
}

func prepare(r Record, old *Record, o Options, now time.Time) (Record, []byte, error) {
	if !validReference(r.KeyID, r.ResponseID) || !r.CodexBridge.Valid() || len(r.OpaqueState) > o.MaxRecordBytes {
		return Record{}, nil, ErrInvalid
	}
	if old == nil {
		// Callers may supply either zero (create) or one (already-normalized
		// record); storage owns the first version and always normalizes it to 1.
		if r.Version > 1 {
			return Record{}, nil, ErrInvalid
		}
		r.Version = 1
		r.CreatedAt = now.UTC()
	} else {
		if err := checkOwner(*old, Record{KeyID: r.KeyID, AccountID: r.AccountID, Provider: r.Provider, InstanceID: r.InstanceID, ConnectionID: r.ConnectionID, SessionFingerprint: Fingerprint("session", r.SessionID), IdentityHash: r.IdentityHash, Scope: r.Scope, CodexBridge: r.CodexBridge, Version: old.Version + 1}); err != nil {
			return Record{}, nil, err
		}
		if r.Version == 0 && bytes.Equal(r.OpaqueState, old.OpaqueState) {
			payload, err := json.Marshal(old)
			return old.clone(), payload, err
		}
		if r.Version != old.Version {
			return Record{}, nil, ErrInvalid
		}
		r.Version = old.Version + 1
		r.CreatedAt = old.CreatedAt
	}
	r.SchemaVersion = 1
	r.ExpiresAt = now.Add(o.TTL).UTC()
	r.ResponseFingerprint = Fingerprint("response", r.ResponseID)
	r.SessionFingerprint = Fingerprint("session", r.SessionID)
	if !r.valid() {
		return Record{}, nil, ErrInvalid
	}
	payload, err := json.Marshal(r)
	if err != nil || len(payload) > o.MaxRecordBytes {
		return Record{}, nil, ErrInvalid
	}
	return r.clone(), payload, nil
}

func decode(payload []byte, responseID string, o Options, now time.Time) (*Record, error) {
	if len(payload) > o.MaxRecordBytes {
		return nil, ErrInvalid
	}
	var r Record
	if json.Unmarshal(payload, &r) != nil || !r.valid() || r.ResponseFingerprint != Fingerprint("response", responseID) || r.SessionFingerprint != Fingerprint("session", r.SessionID) || r.CreatedAt.IsZero() || !r.ExpiresAt.After(r.CreatedAt) {
		return nil, ErrInvalid
	}
	if !now.Before(r.ExpiresAt) {
		return nil, ErrMiss
	}
	r.ResponseID = responseID
	return &r, nil
}

func (r Record) clone() Record {
	r.OpaqueState = append([]byte(nil), r.OpaqueState...)
	return r
}

func checkOwner(old, next Record) error {
	if old.KeyID != next.KeyID || old.AccountID != next.AccountID || old.Provider != next.Provider || old.InstanceID != next.InstanceID || old.ConnectionID != next.ConnectionID || old.SessionFingerprint != next.SessionFingerprint || old.IdentityHash != next.IdentityHash || old.Scope != next.Scope {
		return ErrWrongOwner
	}
	if old.CodexBridge != next.CodexBridge {
		return ErrInvalid
	}
	if next.Version <= old.Version || next.Version-old.Version != 1 {
		return ErrInvalid
	}
	return nil
}

func checkInvalidation(old Record, i Invalidation) error {
	if old.KeyID != i.KeyID || old.InstanceID != i.InstanceID || old.ConnectionID != i.ConnectionID {
		return ErrWrongOwner
	}
	if old.Version != i.Version {
		return ErrInvalid
	}
	return nil
}

func validInvalidation(i Invalidation) bool {
	return validReference(i.KeyID, i.ResponseID) && i.InstanceID != "" && i.ConnectionID != "" && i.Version > 0
}

func unavailable(err error) error {
	// Do not include driver messages: a dial/auth failure may contain a credential
	// or URL. Preserve cancellation classification without exposing connection info.
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("%w: %w", ErrUnavailable, context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %w", ErrUnavailable, context.DeadlineExceeded)
	}
	return ErrUnavailable
}
