package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"pi-gateway/internal/capture"
	"pi-gateway/internal/store"
	"pi-gateway/internal/upstream"
)

// handleChatCompletionsHTTP adapts the public Chat Completions protocol to the
// same Responses pipeline as native clients. Subscription credentials never
// leave the gateway, and the upstream endpoint remains /responses.
func (s *Server) handleChatCompletionsHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	raw, err := readBody(r)
	if err != nil {
		writeError(w, chatInputError(err.Error()))
		return
	}
	converted, options, apiErr := chatRequestToResponses(raw)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	p, apiErr := s.prepareInput(ctx, r, raw, converted, "sse")
	if apiErr != nil {
		if p == nil {
			writeError(w, apiErr)
		} else {
			writeError(captureJSONResponse(w, p.Recorder, "error"), apiErr)
			s.finish(context.WithoutCancel(ctx), p, outcomeFor(apiErr, ctx), apiErr)
		}
		return
	}
	outcome := store.OutcomeOK
	var runErr error
	defer func() { s.finish(context.WithoutCancel(ctx), p, outcome, runErr) }()
	req, err := s.newUpstreamRequest(ctx, p)
	if err != nil {
		outcome, runErr = outcomeFor(err, ctx), err
		writeError(captureJSONResponse(w, p.Recorder, "error"), errorFromUpstream(err))
		return
	}
	outcome, runErr = s.serveChat(w, ctx, p, req, options)
}

func (s *Server) serveChat(w http.ResponseWriter, ctx context.Context, p *prepared, req *upstream.Request, options *chatOptions) (string, error) {
	output := newChatOutput(p.Built.Model)
	delivery := &sseDelivery{writer: w, prepared: p}
	if options.Stream {
		flusher, ok := w.(http.Flusher)
		if !ok {
			err := &apiError{Status: http.StatusInternalServerError, Type: "server_error", Message: "streaming unsupported"}
			writeError(captureJSONResponse(w, p.Recorder, "error"), err)
			return store.OutcomeError, err
		}
		delivery.flusher = flusher
	}
	writeFrame := func(raw []byte, eventType string) error {
		delivery.commit()
		setResponseWriteDeadline(w)
		frame := formatSSEFrame(raw)
		n, err := w.Write(frame)
		if err == nil && n != len(frame) {
			err = io.ErrShortWrite
		}
		if err != nil {
			return err
		}
		if p.Recorder != nil {
			p.Recorder.OnFrame(capture.DirClientOut, store.KindSSEEvent, eventType, raw, nil)
		}
		delivery.flusher.Flush()
		return nil
	}
	writeChunk := func(chunk map[string]any) error {
		if options.IncludeUsage {
			if _, exists := chunk["usage"]; !exists {
				chunk["usage"] = nil
			}
		}
		raw, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		return writeFrame(raw, "chat.completion.chunk")
	}
	var terminal map[string]any
	first := true
	_, err := s.streamWith429Retry(ctx, p, req, func(event *upstream.Event) error {
		if first {
			first = false
			if p.Recorder != nil {
				p.Recorder.NoteTTFB()
			}
		}
		failed := upstreamEventContainsError(event)
		event, transformErr := p.transformResponseEvent(ctx, event)
		if transformErr != nil || event == nil {
			return transformErr
		}
		// Keep failures as HTTP errors until actual chat output is committed.
		// The upstream reader returns the original typed failure after this call,
		// even if a response rule changes or drops the error event.
		if failed {
			return nil
		}
		if upstreamEventContainsError(event) {
			if failure := upstream.FailureFromEvent(event); failure != nil {
				return failure
			}
			return &apiError{Status: http.StatusBadGateway, Type: "upstream_error", Message: "response rule produced an error event"}
		}
		if upstream.IsTerminal(event.Type) {
			terminal = lastEventPayload(event)
		}
		chunks, transformErr := output.event(event)
		if transformErr != nil {
			return transformErr
		}
		if options.Stream {
			for _, chunk := range chunks {
				if err := writeChunk(chunk); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err == nil && terminal == nil {
		err = &apiError{Status: http.StatusBadGateway, Type: "upstream_error", Message: "upstream returned no response terminal event"}
	}
	if err == nil && !options.Stream {
		var payload map[string]any
		payload, err = p.transformResponseBody(ctx, terminal)
		if err == nil {
			payload, err = output.completion(payload)
		}
		if err == nil {
			copyResponseHeaders(w, p.ResponseHeaders)
			writeJSON(captureJSONResponse(w, p.Recorder, "chat.completion"), http.StatusOK, payload)
		}
	}
	if err == nil && options.Stream {
		if options.IncludeUsage {
			if usage := output.usageChunk(); usage != nil {
				err = writeChunk(usage)
			}
		}
		if err == nil {
			err = writeFrame([]byte("[DONE]"), "done")
		}
	}
	if err != nil {
		apiErr := errorFromUpstream(err, p.ResponseHeaders)
		if !delivery.wroteHeaders {
			writeError(captureJSONResponse(w, p.Recorder, "error"), apiErr)
		} else if ctx.Err() == nil {
			// Chat clients expect the standard error envelope, not Responses events.
			payload := streamErrorPayload(err, p.ResponseHeaders)
			raw, _ := json.Marshal(map[string]any{"error": payload["error"]})
			if writeFrame(raw, "error") == nil {
				_ = writeFrame([]byte("[DONE]"), "done")
			}
		}
		return outcomeFor(err, ctx), err
	}
	return store.OutcomeOK, nil
}
