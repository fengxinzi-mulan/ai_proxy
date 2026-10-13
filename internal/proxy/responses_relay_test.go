package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ai_proxy/internal/model"
)

const responsesTextDelta = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"
const responsesCompleted = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"model\":\"m\",\"usage\":{\"input_tokens\":30,\"output_tokens\":7,\"total_tokens\":37}}}\n\n"

type responsesTestBody struct {
	chunks []string
	err    error
	reads  int
	closed bool
}

func (b *responsesTestBody) Read(p []byte) (int, error) {
	b.reads++
	if len(b.chunks) == 0 {
		if b.err != nil {
			return 0, b.err
		}
		return 0, io.EOF
	}
	n := copy(p, b.chunks[0])
	b.chunks[0] = b.chunks[0][n:]
	if b.chunks[0] == "" {
		b.chunks = b.chunks[1:]
	}
	return n, nil
}

func (b *responsesTestBody) Close() error {
	b.closed = true
	return nil
}

type responsesTestWriter struct {
	*httptest.ResponseRecorder
	onTerminal func()
	writeErr   error
	flushErr   error
	shortWrite bool
}

func (w *responsesTestWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "response.completed") {
		if w.writeErr != nil {
			if w.onTerminal != nil {
				w.onTerminal()
			}
			return 0, w.writeErr
		}
		if w.shortWrite {
			return len(p) - 1, nil
		}
	}
	return w.ResponseRecorder.Write(p)
}

func (w *responsesTestWriter) FlushError() error {
	if strings.Contains(w.Body.String(), "response.completed") {
		if w.onTerminal != nil {
			w.onTerminal()
		}
		if w.flushErr != nil {
			return w.flushErr
		}
	}
	w.ResponseRecorder.Flush()
	return nil
}

func TestResponsesRelayCompletedAfterClientCancel(t *testing.T) {
	for _, split := range []string{"whole", "bytewise", "same-chunk-tail"} {
		t.Run(split, func(t *testing.T) {
			srv, st := newTestServer(t)
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			ctx, cancel := context.WithCancel(req.Context())
			defer cancel()
			req = req.WithContext(ctx)
			writer := &responsesTestWriter{ResponseRecorder: httptest.NewRecorder(), onTerminal: cancel}
			body := &responsesTestBody{chunks: []string{responsesTextDelta + responsesCompleted, "unexpected tail"}, err: context.Canceled}
			if split == "bytewise" {
				body.chunks = strings.Split(responsesTextDelta+responsesCompleted, "")
				body.chunks = append(body.chunks, "unexpected tail")
			} else if split == "same-chunk-tail" {
				body.chunks[0] += "event: response.failed\ndata: {\"type\":\"response.failed\"}\n\n"
			}
			in := forwardInput{start: time.Now(), settings: model.DefaultSettings(), provider: model.Provider{APIFormat: model.FormatResponses}, wantStream: true}
			resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body}
			res := srv.relay(writer, req, resp, in, "test")
			res.status = 200
			srv.record(in, res)
			entry := lastLog(t, st)
			if !entry.Success || entry.Cancelled || entry.HTTPStatus != 200 || entry.ErrorMsg != "" {
				t.Fatalf("completed delivery must win over late cancellation: %+v", entry)
			}
			if ctx.Err() != context.Canceled || !res.terminalDelivered || !body.closed || len(body.chunks) != 1 {
				t.Fatalf("must close without reading beyond terminal: result=%+v body=%+v ctx=%v", res, body, ctx.Err())
			}
			if writer.Body.String() != responsesTextDelta+responsesCompleted || entry.RespBody != writer.Body.String() {
				t.Fatal("events through completed must be relayed verbatim")
			}
			if entry.PromptTokens != 30 || entry.CompletionTokens != 7 || entry.TokensEstimated {
				t.Fatalf("terminal usage must be retained: %+v", entry)
			}
		})
	}
}

func TestForwardResponsesTerminalOutcomes(t *testing.T) {
	tool := "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"name\":\"read\"}}\n\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"{}\"}\n\n" +
		"data: {\"type\":\"response.function_call_arguments.done\"}\n\n"
	for _, tc := range []struct {
		name     string
		body     string
		success  bool
		reason   string
		terminal bool
	}{
		{"completed", responsesTextDelta + responsesCompleted, true, "", true},
		{"data-only-tool", tool + strings.ReplaceAll(responsesCompleted, "event: response.completed\n", ""), true, "", true},
		{"incomplete", "event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n", false, "max_output_tokens", true},
		{"incomplete-no-reason", "data: {\"type\":\"response.incomplete\"}\n\n", false, "Responses", true},
		{"failed", "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"overloaded\"}}}\n\n", false, "overloaded", true},
		{"failed-no-reason", "event: response.failed\ndata: {\"type\":\"response.failed\"}\n\n", false, "Responses", true},
		{"completed-with-error", "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"error\":{\"message\":\"business error\"}}}\n\n", false, "business error", true},
		{"truncated-text", responsesTextDelta, false, "", false},
		{"empty-eof", "", false, "", false},
		{"done-only", "data: [DONE]\n\n", false, "", false},
		{"local-done", "data: {\"type\":\"response.output_text.done\"}\n\n", false, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			upstreamClosed := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(upstreamClosed)
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, tc.body)
				w.(http.Flusher).Flush()
				if tc.terminal {
					select {
					case <-r.Context().Done():
					case <-release:
					}
				}
			}))
			defer upstream.Close()
			defer close(release)
			srv, st := newTestServer(t)
			addProvider(t, st, upstream.URL, model.FormatResponses)
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","stream":true,"input":"hello"}`))
			ctx, cancel := context.WithTimeout(req.Context(), 3*time.Second)
			defer cancel()
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req.WithContext(ctx))
			entry := lastLog(t, st)
			if ctx.Err() != nil || entry.Success != tc.success || entry.Cancelled || entry.HTTPStatus != 200 {
				t.Fatalf("wrong outcome or waited for EOF: %+v ctx=%v", entry, ctx.Err())
			}
			if !tc.success && (entry.ErrorMsg == "" || !strings.Contains(entry.ErrorMsg, tc.reason)) {
				t.Fatalf("missing failure reason %q: %q", tc.reason, entry.ErrorMsg)
			}
			if rec.Body.String() != tc.body {
				t.Fatal("response must be relayed verbatim")
			}
			select {
			case <-upstreamClosed:
			case <-time.After(3 * time.Second):
				t.Fatal("upstream body was not closed after terminal delivery")
			}
		})
	}
}

func TestResponsesRelayDeliveryFailure(t *testing.T) {
	for _, tc := range []struct {
		name      string
		writeErr  error
		flushErr  error
		short     bool
		cancel    bool
		cancelled bool
	}{
		{"write-error", io.ErrClosedPipe, nil, false, false, false},
		{"short-write", nil, nil, true, false, false},
		{"flush-error", nil, io.ErrClosedPipe, false, false, false},
		{"cancel-on-write-error", context.Canceled, nil, false, true, true},
		{"cancel-on-flush-error", nil, io.ErrClosedPipe, false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, st := newTestServer(t)
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			ctx, cancel := context.WithCancel(req.Context())
			defer cancel()
			writer := &responsesTestWriter{ResponseRecorder: httptest.NewRecorder(), writeErr: tc.writeErr, flushErr: tc.flushErr, shortWrite: tc.short}
			if tc.cancel {
				writer.onTerminal = cancel
			}
			body := &responsesTestBody{chunks: []string{responsesTextDelta + responsesCompleted}}
			in := forwardInput{start: time.Now(), settings: model.DefaultSettings(), provider: model.Provider{APIFormat: model.FormatResponses}}
			res := srv.relay(writer, req.WithContext(ctx), &http.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body}, in, "test")
			res.status = 200
			srv.record(in, res)
			entry := lastLog(t, st)
			if entry.Success || entry.Cancelled != tc.cancelled || entry.ErrorMsg == "" || res.terminalDelivered {
				t.Fatalf("failed terminal delivery must not count as completed: %+v result=%+v", entry, res)
			}
		})
	}
}

func TestResponsesRelayInterrupted(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		cancel    bool
		cancelled bool
	}{
		{"client-cancel", context.Canceled, true, true},
		{"proxy-timeout", context.DeadlineExceeded, false, false},
		{"upstream-cancel", context.Canceled, false, false},
		{"unexpected-eof", io.ErrUnexpectedEOF, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, st := newTestServer(t)
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			ctx, cancel := context.WithCancel(req.Context())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			body := &responsesTestBody{chunks: []string{responsesTextDelta}, err: tc.err}
			in := forwardInput{start: time.Now(), settings: model.DefaultSettings(), provider: model.Provider{APIFormat: model.FormatResponses}}
			res := srv.relay(httptest.NewRecorder(), req.WithContext(ctx), &http.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body}, in, "test")
			res.status = 200
			srv.record(in, res)
			entry := lastLog(t, st)
			if entry.Success || entry.Cancelled != tc.cancelled || entry.ErrorMsg == "" {
				t.Fatalf("wrong interrupted outcome: %+v", entry)
			}
			if !tc.cancel && !strings.Contains(entry.ErrorMsg, tc.err.Error()) {
				t.Fatalf("read error must be preserved: %q", entry.ErrorMsg)
			}
		})
	}
}

func TestIsClientGoneDeadline(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx, cancel := context.WithDeadline(req.Context(), time.Now().Add(-time.Second))
	defer cancel()
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) || isClientGone(req.WithContext(ctx), ctx.Err()) {
		t.Fatal("a deadline must not be classified as client cancellation")
	}
}

func TestForwardResponsesProxyTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, responsesTextDelta)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	srv, st := newTestServer(t)
	prov := addProvider(t, st, upstream.URL, model.FormatResponses)
	prov.TimeoutSeconds = 1
	if _, err := st.UpdateProvider(prov); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","stream":true,"input":"hello"}`))
	srv.ServeHTTP(httptest.NewRecorder(), req)
	entry := lastLog(t, st)
	if entry.Success || entry.Cancelled || entry.HTTPStatus != 200 || !strings.Contains(entry.ErrorMsg, context.DeadlineExceeded.Error()) {
		t.Fatalf("proxy timeout after headers must be failed 200, not cancelled: %+v", entry)
	}
}

func TestResponsesRelayCompressedPassthrough(t *testing.T) {
	srv, st := newTestServer(t)
	in := forwardInput{start: time.Now(), settings: model.DefaultSettings(), provider: model.Provider{APIFormat: model.FormatResponses}}
	body := &responsesTestBody{chunks: []string{"opaque compressed bytes"}}
	resp := &http.Response{Header: http.Header{"Content-Type": {"text/event-stream"}, "Content-Encoding": {"gzip"}}, Body: body}
	writer := httptest.NewRecorder()
	res := srv.relay(writer, httptest.NewRequest(http.MethodPost, "/v1/responses", nil), resp, in, "test")
	res.status = 200
	srv.record(in, res)
	entry := lastLog(t, st)
	if !entry.Success || writer.Body.String() != "opaque compressed bytes" || !entry.TokensEstimated {
		t.Fatalf("unparseable compressed responses must retain passthrough behavior: %+v", entry)
	}
}
