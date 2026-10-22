package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"pi-gateway/internal/piwire"
)

// Pi's custom Codex transport is not the SDK ResponsesWS implementation:
// _research/pi/packages/ai/src/api/openai-codex-responses.ts:553-564 sets
// stream:true/store:false, and :1542 sends {type:"response.create", ...requestBody}.
// This fixture checks the envelope operation, not the gateway's separate HTTP
// body policy, which filters fields rejected by the preview route.
func TestBuildWSFrameMatchesPiCodexEnvelope(t *testing.T) {
	const piBody = `{"model":"gpt-5.1-codex","store":false,"stream":true,"instructions":"You are a helpful assistant.","input":[],"text":{"verbosity":"low"},"include":["reasoning.encrypted_content"],"tool_choice":"auto","parallel_tool_calls":true}`
	for _, tc := range []struct {
		name  string
		extra string
	}{
		{name: "no_session"},
		{name: "with_session", extra: `,"prompt_cache_key":"sess-1"`},
		{name: "cached_continuation", extra: `,"prompt_cache_key":"sess-1","previous_response_id":"resp-1"`},
		{name: "optional_fields", extra: `,"temperature":0.2,"service_tier":"priority","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"reasoning":{"effort":"high","summary":"auto"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(piBody[:len(piBody)-1] + tc.extra + "}")
			original := bytes.Clone(body)
			frame, err := buildWSFrame(body)
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
			var want, got map[string]any
			{
				err := json.Unmarshal(original, &want)
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
			want["type"] = "response.create"
			{
				err := json.Unmarshal(frame, &got)
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
			// DeepEqual asserts the complete key set as well as every value; no
			// SDK-only stream_id/background field may be injected or removed.
			reportTestFailure(t, !reflect.DeepEqual(got, want), func() {
				t.Fatalf("Pi Codex envelope mismatch\n got: %#v\nwant: %#v", got, want)
			})
			reportTestFailure(t, got["stream"] != true || got["store"] != false, func() {
				t.Fatalf("Pi Codex requires stream:true/store:false: %s", frame)
			})
			reportTestFailure(t, !bytes.Equal(frame, append([]byte(`{"type":"response.create",`), original[1:]...)), func() {
				t.Fatalf("body field order changed: %s", frame)
			})
			reportTestFailure(t, !bytes.Equal(body, original), func() {
				t.Fatal("WS framing mutated the HTTP body")
			})
		})
	}
}

func TestBuildWSFramePreservesBuildRequestAndEnforcePiShape(t *testing.T) {
	for _, session := range []string{"", "wire-session"} {
		t.Run("session="+session, func(t *testing.T) {
			client := map[string]any{
				"model": "gpt-5.1-codex", "input": []any{}, "stream": false, "store": true,
				"instructions": "preserve client instructions", "text": map[string]any{"verbosity": "high"}, "parallel_tool_calls": false,
			}
			built, err := piwire.BuildRequest(client, piwire.BuildOptions{SessionID: session})
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
			// Keep middleware shape enforcement intact before transport framing.
			built.Body.Set("stream", false)
			built.Body.Set("store", true)
			built.Body.Set("temperature", 0.2)
			built.Body.Set("metadata", map[string]any{"unsupported": true})
			piwire.EnforcePiShape(built.Body)
			httpBody, err := json.Marshal(built.Body)
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
			reportTestFailure(t, !bytes.Equal(httpBody, built.JSON), func() {
				t.Fatalf("EnforcePiShape regression: %s != %s", httpBody, built.JSON)
			})
			frame, err := buildWSFrame(httpBody)
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
			want := map[string]any{"type": "response.create", "model": "gpt-5.1-codex", "input": []any{}, "stream": true, "store": false}
			for _, field := range []string{"instructions", "text", "parallel_tool_calls"} {
				want[field] = client[field]
			}
			if session != "" {
				want["prompt_cache_key"] = session
			}
			var got map[string]any
			{
				err := json.Unmarshal(frame, &got)
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
			reportTestFailure(t, !reflect.DeepEqual(got, want), func() {
				t.Fatalf("gateway envelope mismatch\n got: %#v\nwant: %#v", got, want)
			})
			reportTestFailure(t, !bytes.Equal(httpBody, built.JSON), func() {
				t.Fatal("WS framing changed the HTTP SSE body")
			})
		})
	}
}

func TestSSEBodyRetainsStreamAfterWSFraming(t *testing.T) {
	built, err := piwire.BuildRequest(map[string]any{"model": "gpt-5.1-codex", "input": []any{}, "stream": false}, piwire.BuildOptions{})
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	original := bytes.Clone(built.JSON)
	{
		_, err := buildWSFrame(built.JSON)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	captured := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		reportTestFailure(t, err != nil, func() {
			t.Errorf("read HTTP SSE body: %v", err)
		})
		captured <- body
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"status\":\"completed\"}}\n\n")
	}))
	defer server.Close()
	c := testClient(t, server.URL)
	{
		_, err := c.Stream(context.Background(), &Request{Transport: "sse", Body: built.JSON}, func(*Event) error { return nil })
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	var sent []byte
	select {
	case sent = <-captured:
	default:
		t.Fatal("no HTTP SSE request captured")
	}
	reportTestFailure(t, !bytes.Equal(sent, original), func() {
		t.Fatalf("HTTP body changed\n got: %s\nwant: %s", sent, original)
	})
	var body map[string]any
	{
		err := json.Unmarshal(sent, &body)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	reportTestFailure(t, body["stream"] != true, func() {
		t.Fatalf("HTTP SSE must retain stream:true: %s", sent)
	})
	{
		_, ok := body["type"]
		reportTestFailure(t, ok, func() {
			t.Fatalf("WS envelope type leaked into HTTP SSE body: %s", sent)
		})
	}
}
