package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"pi-gateway/internal/accounts"
	"pi-gateway/internal/store"
	"pi-gateway/internal/upstream"
)

// switchOn429 applies enabled group overrides within the requesting key's scope.
// Disabled wins when multiple applicable groups disagree.
func (s *Server) switchOn429(ctx context.Context, p *prepared) bool {
	enabled := s.settings.Get().SwitchOn429
	groups, err := s.store.ListAccountGroups(ctx)
	if err != nil {
		return false
	}
	for _, g := range groups {
		if !g.Enabled || !containsID(p.Account.GroupIDs, g.ID) || (len(p.Key.GroupIDs) > 0 && !containsID(p.Key.GroupIDs, g.ID)) {
			continue
		}
		if g.SwitchOn429 == "disabled" {
			return false
		}
		if g.SwitchOn429 == "enabled" {
			enabled = true
		}
	}
	return enabled
}

func containsID(ids []int64, id int64) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

func retryAfterDuration(h http.Header, now time.Time) time.Duration {
	var delay time.Duration
	if n, err := strconv.ParseFloat(h.Get("Retry-After-Ms"), 64); err == nil && n > 0 && n <= 604800000 {
		delay = time.Duration(n * float64(time.Millisecond))
	}
	if n, err := strconv.ParseFloat(h.Get("Retry-After"), 64); err == nil && n > 0 && n <= 604800 {
		delay = max(delay, time.Duration(n*float64(time.Second)))
	} else if at, err := http.ParseTime(h.Get("Retry-After")); err == nil {
		delay = max(delay, at.Sub(now))
	}
	return delay
}

const sameAccount429WaitTimeout = 30 * time.Second

func (s *Server) streamWith429Retry(ctx context.Context, p *prepared, req *upstream.Request, onEvent func(*upstream.Event) error) (*upstream.StreamResult, error) {
	return s.streamWith429RetryWindow(ctx, p, req, onEvent, sameAccount429WaitTimeout)
}

// One deadline is shared by every cooldown/concurrency wait for this request.
// Acquiring a slot does not reset the budget or limit successful generation.
func (s *Server) streamWith429RetryWindow(ctx context.Context, p *prepared, req *upstream.Request, onEvent func(*upstream.Event) error, waitTimeout time.Duration) (*upstream.StreamResult, error) {
	attempts := max(1, s.settings.Get().MaxAttempts)
	excluded := map[int64]bool{}
	var waitCtx context.Context
	var cancelWait context.CancelFunc
	defer func() {
		if cancelWait != nil {
			cancelWait()
		}
	}()
	for attempt := 1; ; attempt++ {
		mayRetry := attempt < attempts && p.Continuation == nil && p.PreviousResponseID == ""
		maySwitch := mayRetry && s.switchOn429(ctx, p)
		delivered := false
		var pending *upstream.Event
		result, err := s.upstream.Stream(ctx, req, func(event *upstream.Event) error {
			if mayRetry && !delivered {
				if failure := upstream.FailureFromEvent(event); failure != nil && errorFromUpstream(failure).Status == 429 {
					pending = event
					return nil
				}
			}
			delivered = true
			return onEvent(event)
		})
		now := time.Now()
		status := 200
		message := ""
		if err != nil {
			status = errorFromUpstream(err).Status
			message = err.Error()
		}
		if status == 429 && mayRetry && !maySwitch && !delivered && waitCtx == nil {
			waitCtx, cancelWait = context.WithTimeout(ctx, waitTimeout)
		}
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		healthStatus := status
		var failure *upstream.FailureError
		if errors.As(err, &failure) && failure.OperationDenied {
			healthStatus = 0
		}
		recordErr := s.accounts.RecordResult(recordCtx, p.Account.ID, accounts.Result{Failed: err != nil, StatusCode: healthStatus, RetryAfter: retryAfterDuration(p.ResponseHeaders, now), Error: message}, now)
		cancel()
		if recordErr != nil {
			s.logger.Warn("recording account attempt failed", "account", p.Account.ID, "error", recordErr)
		}
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if err == nil || status != 429 || !mayRetry || delivered || (recordErr != nil && !maySwitch) {
			if pending != nil {
				if deliveryErr := onEvent(pending); deliveryErr != nil {
					return result, deliveryErr
				}
			}
			return result, err
		}
		if p.AccountRelease != nil {
			p.AccountRelease()
			p.AccountRelease = nil
		}
		var next *store.Account
		var release func()
		var acquireErr error
		if maySwitch {
			excluded[p.Account.ID] = true
			next, release, acquireErr = s.accounts.AcquireExcluding(ctx, p.Key, p.Built.Model, "", excluded)
		} else {
			// Release first, then join the original account's FIFO queue. The
			// manager rechecks current cooldown, concurrency and authorization.
			next, release, acquireErr = s.accounts.AcquireRetry(waitCtx, p.Key, p.Account.ID, p.Built.Model)
		}
		if acquireErr != nil {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			if pending != nil {
				if deliveryErr := onEvent(pending); deliveryErr != nil {
					return result, deliveryErr
				}
			}
			// A spent wait budget preserves the upstream 429 and retry headers.
			return result, err
		}
		// Preserve a separate usage entry for the failed attempt before replaying.
		if p.Usage != nil {
			p.Usage.finalize(context.WithoutCancel(ctx), store.OutcomeError, message)
		}
		_ = s.store.MarkAccountUsed(context.WithoutCancel(ctx), p.Account.ID, true)
		p.Account, p.AccountRelease = next, release
		p.Release = composeRelease(p.Release, release)
		clientTransport := "sse"
		if p.Usage != nil {
			clientTransport = p.Usage.clientTransport
		}
		p.Transport = s.resolveTransport(s.settings.Get(), p.Key, next, clientTransport)
		if p.Compact {
			p.Transport = "sse"
		}
		previousHeaders := p.ResponseHeaders
		p.ResponseHeaders = nil
		p.RuleContext["account_id"], p.RuleContext["upstream_protocol"] = next.ID, p.Transport
		if p.Recorder != nil {
			p.Recorder.SetRoute(next.ID, next.Name, p.Built.Model, p.SessionID, p.Transport)
		}
		p.Usage = s.newUsageTracker(p.Key, next, p.Built.Model, clientTransport, p.Transport, p.SessionID, stringField(p.ClientBody, "service_tier"))
		p.Usage.noteRequestMetadata(p.Built.JSON)
		if p.Compact {
			p.Usage.requestKind = "compaction"
		}
		s.startUsage(ctx, p.Usage)
		buildCtx := ctx
		if !maySwitch {
			buildCtx = waitCtx
		}
		var buildErr error
		req, buildErr = s.newUpstreamRequest(buildCtx, p)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !maySwitch && waitCtx.Err() != nil {
			// Token/rule preparation also must not send a replay after the
			// shared deadline, or discard the last upstream retry metadata.
			p.ResponseHeaders = previousHeaders
			if pending != nil {
				if deliveryErr := onEvent(pending); deliveryErr != nil {
					return result, deliveryErr
				}
			}
			return result, err
		}
		if buildErr != nil {
			return nil, buildErr
		}
	}
}
