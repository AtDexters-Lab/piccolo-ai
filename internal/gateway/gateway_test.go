package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testToken = "test-token-with-at-least-24-characters"

func newTestHandler(t *testing.T, backend http.Handler) *Handler {
	t.Helper()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(Config{APIToken: testToken, Backend: target, ModelName: "piccolo-chat", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func TestV3RequiresExactlyOneValidBearerToken(t *testing.T) {
	handler := newTestHandler(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	tests := []struct {
		name    string
		headers []string
		want    int
	}{
		{name: "missing", want: http.StatusUnauthorized},
		{name: "wrong", headers: []string{"Bearer wrong"}, want: http.StatusUnauthorized},
		{name: "wrong scheme", headers: []string{testToken}, want: http.StatusUnauthorized},
		{name: "duplicates", headers: []string{"Bearer " + testToken, "Bearer " + testToken}, want: http.StatusUnauthorized},
		{name: "valid", headers: []string{"Bearer " + testToken}, want: http.StatusNoContent},
		{name: "case insensitive scheme", headers: []string{"bearer " + testToken}, want: http.StatusNoContent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/v3/models", nil)
			for _, value := range tt.headers {
				request.Header.Add("Authorization", value)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tt.want {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, tt.want, response.Body.String())
			}
		})
	}
}

func TestProxyStripsCredentialsAndPiccoloIdentity(t *testing.T) {
	handler := newTestHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, name := range []string{
			"Authorization",
			"Cookie",
			"Forwarded",
			"X-Forwarded-For",
			"X-Forwarded-Port",
			"X-Real-IP",
			"X-Piccolo-User",
			"X-Piccolo-Role",
			"X-Piccolo-Hint-Token",
		} {
			if value := r.Header.Get(name); value != "" {
				t.Errorf("backend received %s=%q", name, value)
			}
		}
		if r.URL.Path != "/v3/chat/completions" || r.URL.RawQuery != "trace=1" {
			t.Errorf("backend URL = %s, want /v3/chat/completions?trace=1", r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))

	request := httptest.NewRequest(http.MethodPost, "/v3/chat/completions?trace=1", strings.NewReader(`{}`))
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("Cookie", "session=secret")
	request.Header.Set("Forwarded", "for=203.0.113.10")
	request.Header.Set("X-Forwarded-For", "203.0.113.10")
	request.Header.Set("X-Forwarded-Port", "443")
	request.Header.Set("X-Real-IP", "203.0.113.10")
	request.Header.Set("X-Piccolo-User", "user")
	request.Header.Set("X-Piccolo-Role", "admin")
	request.Header.Set("X-Piccolo-Hint-Token", "spoofed")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
}

func TestProxyStreamsWithoutBuffering(t *testing.T) {
	firstFlushed := make(chan struct{})
	releaseSecond := make(chan struct{})
	handler := newTestHandler(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("backend response writer does not support flushing")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		flusher.Flush()
		close(firstFlushed)
		<-releaseSecond
		_, _ = io.WriteString(w, "data: second\n\n")
		flusher.Flush()
	}))

	server := httptest.NewServer(handler)
	defer server.Close()
	request, err := http.NewRequest(http.MethodGet, server.URL+"/v3/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	select {
	case <-firstFlushed:
	case <-time.After(time.Second):
		t.Fatal("backend did not flush first event")
	}
	line, err := bufio.NewReader(response.Body).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != "data: first\n" {
		t.Fatalf("first streamed line = %q", line)
	}
	close(releaseSecond)
}

func TestStreamingRequestEmitsHeartbeatBeforeBackendResponse(t *testing.T) {
	backendStarted := make(chan struct{})
	releaseBackend := make(chan struct{})
	handler := newTestHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(backendStarted)
		<-releaseBackend
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
	}))
	handler.streamHeartbeatInterval = 10 * time.Millisecond

	server := httptest.NewServer(handler)
	defer server.Close()
	released := false
	defer func() {
		if !released {
			close(releaseBackend)
		}
	}()

	request, err := http.NewRequest(
		http.MethodPost,
		server.URL+"/v3/chat/completions",
		strings.NewReader(`{"stream":true}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	response, err := (&http.Client{Timeout: time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	select {
	case <-backendStarted:
	case <-time.After(time.Second):
		t.Fatal("backend did not consume the request body")
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if contentType := response.Header.Get("Content-Type"); contentType != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", contentType)
	}
	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != ": keepalive\n" {
		t.Fatalf("first streamed line = %q, want heartbeat comment", line)
	}
	if line, err = reader.ReadString('\n'); err != nil || line != "\n" {
		t.Fatalf("heartbeat terminator = %q, err=%v", line, err)
	}

	close(releaseBackend)
	released = true
	for {
		line, err = reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "data: first\n" {
			break
		}
	}
}

func TestUnaryRequestDoesNotEmitHeartbeat(t *testing.T) {
	backendStarted := make(chan struct{})
	releaseBackend := make(chan struct{})
	handler := newTestHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(backendStarted)
		<-releaseBackend
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	handler.streamHeartbeatInterval = 10 * time.Millisecond

	server := httptest.NewServer(handler)
	defer server.Close()
	request, err := http.NewRequest(
		http.MethodPost,
		server.URL+"/v3/chat/completions",
		strings.NewReader(`{"stream":false}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)

	type requestResult struct {
		response *http.Response
		err      error
	}
	resultChannel := make(chan requestResult, 1)
	go func() {
		response, err := (&http.Client{Timeout: time.Second}).Do(request)
		resultChannel <- requestResult{response: response, err: err}
	}()

	select {
	case <-backendStarted:
	case <-time.After(time.Second):
		t.Fatal("backend did not consume the request body")
	}
	select {
	case result := <-resultChannel:
		if result.response != nil {
			result.response.Body.Close()
		}
		t.Fatalf("unary response arrived before backend release: %v", result.err)
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseBackend)
	got := <-resultChannel
	if got.err != nil {
		t.Fatal(got.err)
	}
	defer got.response.Body.Close()
	if contentType := got.response.Header.Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", contentType)
	}
}

func TestStreamingHeartbeatContinuesBetweenBackendEvents(t *testing.T) {
	releaseBackend := make(chan struct{})
	handler := newTestHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("backend response writer does not support flushing")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		flusher.Flush()
		<-releaseBackend
		_, _ = io.WriteString(w, "data: second\n\n")
		flusher.Flush()
	}))
	handler.streamHeartbeatInterval = 10 * time.Millisecond

	server := httptest.NewServer(handler)
	defer server.Close()
	released := false
	defer func() {
		if !released {
			close(releaseBackend)
		}
	}()
	request, err := http.NewRequest(
		http.MethodPost,
		server.URL+"/v3/chat/completions",
		strings.NewReader(`{"stream":true}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	response, err := (&http.Client{Timeout: time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	reader := bufio.NewReader(response.Body)
	sawFirst := false
	sawHeartbeatAfterFirst := false
	deadline := time.Now().Add(time.Second)
	for !sawHeartbeatAfterFirst && time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		switch line {
		case "data: first\n":
			sawFirst = true
		case ": keepalive\n":
			if sawFirst {
				sawHeartbeatAfterFirst = true
			}
		}
	}
	if !sawHeartbeatAfterFirst {
		t.Fatal("heartbeat was not emitted while backend stream was idle")
	}

	close(releaseBackend)
	released = true
}

func TestStreamingHeartbeatDoesNotSplitBackendSSELine(t *testing.T) {
	firstFlushed := make(chan struct{})
	releaseRemainder := make(chan struct{})
	releaseBackend := make(chan struct{})
	handler := newTestHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: part")
		flusher.Flush()
		close(firstFlushed)
		<-releaseRemainder
		_, _ = io.WriteString(w, "ial\n\n")
		flusher.Flush()
		<-releaseBackend
	}))
	handler.streamHeartbeatInterval = 10 * time.Millisecond

	server := httptest.NewServer(handler)
	defer server.Close()
	remainderReleased := false
	backendReleased := false
	defer func() {
		if !remainderReleased {
			close(releaseRemainder)
		}
		if !backendReleased {
			close(releaseBackend)
		}
	}()
	request, err := http.NewRequest(
		http.MethodPost,
		server.URL+"/v3/chat/completions",
		strings.NewReader(`{"stream":true}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	response, err := (&http.Client{Timeout: time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	select {
	case <-firstFlushed:
	case <-time.After(time.Second):
		t.Fatal("backend did not flush partial SSE line")
	}
	time.Sleep(30 * time.Millisecond)
	close(releaseRemainder)
	remainderReleased = true

	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != "data: partial\n" {
		t.Fatalf("backend SSE line was corrupted: %q", line)
	}
	if line, err = reader.ReadString('\n'); err != nil || line != "\n" {
		t.Fatalf("backend event terminator = %q, err=%v", line, err)
	}
	line, err = reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != ": keepalive\n" {
		t.Fatalf("line after idle backend event = %q, want heartbeat", line)
	}

	close(releaseBackend)
	backendReleased = true
}

func TestStreamingHeartbeatDoesNotSplitBackendSSEEvent(t *testing.T) {
	firstLineFlushed := make(chan struct{})
	releaseRemainder := make(chan struct{})
	releaseBackend := make(chan struct{})
	handler := newTestHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: message\n")
		flusher.Flush()
		close(firstLineFlushed)
		<-releaseRemainder
		_, _ = io.WriteString(w, "data: payload\n\n")
		flusher.Flush()
		<-releaseBackend
	}))
	handler.streamHeartbeatInterval = 10 * time.Millisecond

	server := httptest.NewServer(handler)
	defer server.Close()
	remainderReleased := false
	backendReleased := false
	defer func() {
		if !remainderReleased {
			close(releaseRemainder)
		}
		if !backendReleased {
			close(releaseBackend)
		}
	}()
	request, err := http.NewRequest(
		http.MethodPost,
		server.URL+"/v3/chat/completions",
		strings.NewReader(`{"stream":true}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	response, err := (&http.Client{Timeout: time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	select {
	case <-firstLineFlushed:
	case <-time.After(time.Second):
		t.Fatal("backend did not flush first SSE event line")
	}
	time.Sleep(30 * time.Millisecond)
	close(releaseRemainder)
	remainderReleased = true

	reader := bufio.NewReader(response.Body)
	lines := []string{"event: message\n", "data: payload\n", "\n", ": keepalive\n"}
	for _, want := range lines {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatal(readErr)
		}
		if line != want {
			t.Fatalf("streamed line = %q, want %q", line, want)
		}
	}

	close(releaseBackend)
	backendReleased = true
}

func TestStreamingHeartbeatSurvivesInformationalBackendResponse(t *testing.T) {
	releaseBackend := make(chan struct{})
	handler := newTestHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Interim", "yes")
		w.WriteHeader(http.StatusContinue)
		w.Header().Del("X-Interim")
		_, _ = io.Copy(io.Discard, r.Body)
		<-releaseBackend
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: final\n\n")
	}))
	handler.streamHeartbeatInterval = 10 * time.Millisecond

	server := httptest.NewServer(handler)
	defer server.Close()
	released := false
	defer func() {
		if !released {
			close(releaseBackend)
		}
	}()
	request, err := http.NewRequest(
		http.MethodPost,
		server.URL+"/v3/chat/completions",
		strings.NewReader(`{"stream":true}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("Expect", "100-continue")
	response, err := (&http.Client{Timeout: time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	if value := response.Header.Get("X-Interim"); value != "" {
		t.Fatalf("final response retained informational header X-Interim=%q", value)
	}
	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != ": keepalive\n" {
		t.Fatalf("first final-response line = %q, want heartbeat comment", line)
	}
	if line, err = reader.ReadString('\n'); err != nil || line != "\n" {
		t.Fatalf("heartbeat terminator = %q, err=%v", line, err)
	}

	close(releaseBackend)
	released = true
	for {
		line, err = reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "data: final\n" {
			break
		}
	}
}

func TestStreamingHeartbeatRequestsIdentityEncoding(t *testing.T) {
	observedEncoding := make(chan string, 1)
	releaseBackend := make(chan struct{})
	handler := newTestHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		observedEncoding <- r.Header.Get("Accept-Encoding")
		<-releaseBackend
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: final\n\n")
	}))
	handler.streamHeartbeatInterval = 10 * time.Millisecond

	server := httptest.NewServer(handler)
	defer server.Close()
	released := false
	defer func() {
		if !released {
			close(releaseBackend)
		}
	}()
	request, err := http.NewRequest(
		http.MethodPost,
		server.URL+"/v3/chat/completions",
		strings.NewReader(`{"stream":true}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("Accept-Encoding", "gzip")
	response, err := (&http.Client{Timeout: time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	select {
	case encoding := <-observedEncoding:
		if encoding != "identity" {
			t.Fatalf("backend Accept-Encoding = %q, want identity", encoding)
		}
	case <-time.After(time.Second):
		t.Fatal("backend did not receive request")
	}
	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != ": keepalive\n" {
		t.Fatalf("first streamed line = %q, want heartbeat comment", line)
	}

	close(releaseBackend)
	released = true
}

func TestStreamingHeartbeatRemovesBackendContentLength(t *testing.T) {
	releaseBackend := make(chan struct{})
	const backendEvent = "data: final\n\n"
	handler := newTestHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(backendEvent)))
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-releaseBackend
		_, _ = io.WriteString(w, backendEvent)
	}))
	handler.streamHeartbeatInterval = 10 * time.Millisecond

	server := httptest.NewServer(handler)
	defer server.Close()
	released := false
	defer func() {
		if !released {
			close(releaseBackend)
		}
	}()
	request, err := http.NewRequest(
		http.MethodPost,
		server.URL+"/v3/chat/completions",
		strings.NewReader(`{"stream":true}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	response, err := (&http.Client{Timeout: time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	if contentLength := response.Header.Get("Content-Length"); contentLength != "" {
		t.Fatalf("response Content-Length = %q, want empty", contentLength)
	}
	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != ": keepalive\n" {
		t.Fatalf("first streamed line = %q, want heartbeat", line)
	}

	close(releaseBackend)
	released = true
	remainder, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(remainder), backendEvent) {
		t.Fatalf("backend SSE event was truncated: %q", remainder)
	}
}

func TestProxiedResponseForwardsBackendTrailers(t *testing.T) {
	handler := newTestHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Trailer", "X-Backend-Trailer")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
		w.Header().Set("X-Backend-Trailer", "complete")
	}))

	server := httptest.NewServer(handler)
	defer server.Close()
	request, err := http.NewRequest(
		http.MethodPost,
		server.URL+"/v3/chat/completions",
		strings.NewReader(`{"stream":false}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	response, err := (&http.Client{Timeout: time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatal(err)
	}
	if trailer := response.Trailer.Get("X-Backend-Trailer"); trailer != "complete" {
		t.Fatalf("backend trailer = %q, want complete", trailer)
	}
}

func TestEarlyStreamingHeartbeatForwardsBackendTrailers(t *testing.T) {
	releaseBackend := make(chan struct{})
	handler := newTestHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-releaseBackend
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Trailer", "X-Backend-Trailer")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: final\n\n")
		w.Header().Set("X-Backend-Trailer", "complete")
	}))
	handler.streamHeartbeatInterval = 10 * time.Millisecond

	server := httptest.NewServer(handler)
	defer server.Close()
	released := false
	defer func() {
		if !released {
			close(releaseBackend)
		}
	}()
	request, err := http.NewRequest(
		http.MethodPost,
		server.URL+"/v3/chat/completions",
		strings.NewReader(`{"stream":true}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	response, err := (&http.Client{Timeout: time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != ": keepalive\n" {
		t.Fatalf("first streamed line = %q, want heartbeat", line)
	}

	close(releaseBackend)
	released = true
	if _, err := io.ReadAll(reader); err != nil {
		t.Fatal(err)
	}
	if trailer := response.Trailer.Get("X-Backend-Trailer"); trailer != "complete" {
		t.Fatalf("backend trailer = %q, want complete", trailer)
	}
}

func TestHeartbeatRejectsEncodedBackendStreamAfterCommit(t *testing.T) {
	response := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := newHeartbeatResponseWriter(response, ctx, cancel, time.Second)
	writer.writeMu.Lock()
	writer.commitHeartbeatLocked()
	writer.writeMu.Unlock()

	request := httptest.NewRequest(http.MethodPost, "/v3/chat/completions", nil)
	request = request.WithContext(context.WithValue(request.Context(), heartbeatWriterContextKey{}, writer))
	backendResponse := &http.Response{
		Status:     "200 OK",
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":     {"text/event-stream"},
			"Content-Encoding": {"gzip"},
		},
		Request: request,
	}
	if err := prepareHeartbeatResponse(backendResponse); err == nil {
		t.Fatal("encoded backend stream was accepted after plaintext heartbeat")
	}
}

func TestHeartbeatRejectsEncodedBackendStreamBeforeCommit(t *testing.T) {
	handler := newTestHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = io.WriteString(w, "encoded bytes")
	}))
	handler.streamHeartbeatInterval = time.Second

	request := httptest.NewRequest(
		http.MethodPost,
		"/v3/chat/completions",
		strings.NewReader(`{"stream":true}`),
	)
	request.Header.Set("Authorization", "Bearer "+testToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"code":"backend_unavailable"`) {
		t.Fatalf("unexpected error body: %s", response.Body.String())
	}
	if strings.Contains(response.Body.String(), "encoded bytes") {
		t.Fatalf("encoded backend body leaked into response: %s", response.Body.String())
	}
}

func TestEncodedBackendResponseDisablesLaterHeartbeat(t *testing.T) {
	response := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := newHeartbeatResponseWriter(response, ctx, cancel, 10*time.Millisecond)

	request := httptest.NewRequest(http.MethodPost, "/v3/chat/completions", nil)
	request = request.WithContext(context.WithValue(request.Context(), heartbeatWriterContextKey{}, writer))
	backendResponse := &http.Response{
		Status:     "200 OK",
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":     {"text/event-stream"},
			"Content-Encoding": {"gzip"},
		},
		Request: request,
	}
	if err := prepareHeartbeatResponse(backendResponse); err != nil {
		t.Fatalf("response-first encoding check returned error before stream detection: %v", err)
	}

	writer.enable(true)
	time.Sleep(30 * time.Millisecond)
	writer.stop()
	if body := response.Body.String(); body != "" {
		t.Fatalf("heartbeat started after encoded response was accepted: %q", body)
	}
}

func TestStreamingHeartbeatFramesDelayedBackendErrorAsSSE(t *testing.T) {
	releaseBackend := make(chan struct{})
	handler := newTestHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-releaseBackend
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid image"}}`)
	}))
	handler.streamHeartbeatInterval = 10 * time.Millisecond

	server := httptest.NewServer(handler)
	defer server.Close()
	released := false
	defer func() {
		if !released {
			close(releaseBackend)
		}
	}()
	request, err := http.NewRequest(
		http.MethodPost,
		server.URL+"/v3/chat/completions",
		strings.NewReader(`{"stream":true}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	response, err := (&http.Client{Timeout: time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != ": keepalive\n" {
		t.Fatalf("first streamed line = %q, want heartbeat comment", line)
	}
	if line, err = reader.ReadString('\n'); err != nil || line != "\n" {
		t.Fatalf("heartbeat terminator = %q, err=%v", line, err)
	}

	close(releaseBackend)
	released = true
	remainder, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(remainder), "event: error\ndata: ") {
		t.Fatalf("delayed backend error is not SSE-framed: %q", remainder)
	}
	if !strings.Contains(string(remainder), `"code":"backend_unavailable"`) {
		t.Fatalf("unexpected SSE error payload: %q", remainder)
	}
	if strings.Contains(string(remainder), "invalid image") {
		t.Fatalf("raw backend JSON leaked into SSE stream: %q", remainder)
	}
}

func TestStreamFlagScannerRecognizesOnlyFinalTopLevelBoolean(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "true", body: `{"stream":true}`, want: true},
		{name: "false", body: `{"stream":false}`},
		{name: "nested", body: `{"input":{"stream":true}}`},
		{name: "string content", body: `{"input":"\"stream\":true","stream":false}`},
		{name: "last duplicate wins true", body: `{"stream":false,"stream":true}`, want: true},
		{name: "last duplicate wins false", body: `{"stream":true,"stream":false}`},
		{name: "non boolean", body: `{"stream":"true"}`},
		{name: "whitespace within boolean", body: `{"stream":tr ue}`},
		{name: "trailing comma", body: `{"stream":true,}`},
		{name: "trailing garbage", body: `{"stream":true}x`},
		{name: "escaped key", body: `{"strea\u006d":true}`, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var scanner streamFlagScanner
			for i := range len(tt.body) {
				scanner.feed([]byte{tt.body[i]})
			}
			if got := scanner.streaming(); got != tt.want {
				t.Fatalf("streaming() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestStreamFlagScannerStorageRemainsBounded(t *testing.T) {
	var unrelated streamFlagScanner
	unrelated.feed([]byte(`{"payload":`))
	chunk := []byte(strings.Repeat("1", 4096))
	for range 256 {
		unrelated.feed(chunk)
	}
	if got := len(unrelated.primitive); got != 0 {
		t.Fatalf("unrelated primitive retained %d bytes, want 0", got)
	}

	var stream streamFlagScanner
	stream.feed([]byte(`{"stream":`))
	for range 256 {
		stream.feed(chunk)
	}
	if got, max := len(stream.primitive), len("true")+1; got > max {
		t.Fatalf("stream primitive retained %d bytes, want at most %d", got, max)
	}
}

func TestEscapedStreamKeyComparisonDoesNotAllocate(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		{key: `stream`, want: true},
		{key: `strea\u006d`, want: true},
		{key: `\u0073\u0074\u0072\u0065\u0061\u006d`, want: true},
		{key: `\u0061`},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			scanner := streamFlagScanner{key: []byte(tt.key)}
			if got := scanner.keyIsStream(); got != tt.want {
				t.Fatalf("keyIsStream() = %t, want %t", got, tt.want)
			}
			if allocations := testing.AllocsPerRun(1000, func() {
				_ = scanner.keyIsStream()
			}); allocations != 0 {
				t.Fatalf("keyIsStream() allocations = %f, want 0", allocations)
			}
		})
	}
}

func TestHealthReflectsBackendLiveness(t *testing.T) {
	live := true
	handler := newTestHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/health/live" {
			http.NotFound(w, r)
			return
		}
		if !live {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("live health status = %d", response.Code)
	}

	live = false
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("not-live health status = %d", response.Code)
	}
}

func TestHealthFailsWhenInternalBackendIsUnavailable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	target, err := url.Parse("http://" + address)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(Config{APIToken: testToken, Backend: target, ModelName: "piccolo-chat", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("health status = %d", response.Code)
	}
}

func TestReadinessReflectsBackendReadiness(t *testing.T) {
	ready := false
	handler := newTestHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/models/piccolo-chat/ready" {
			http.NotFound(w, r)
			return
		}
		if !ready {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("not-ready status = %d", response.Code)
	}

	ready = true
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("ready status = %d", response.Code)
	}
}

func TestUnavailableHandlerReturnsRetryableOpenAIError(t *testing.T) {
	handler := NewUnavailableHandler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v3/chat/completions", strings.NewReader(`{}`)))

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", response.Code)
	}
	if response.Header().Get("Retry-After") != "5" {
		t.Fatalf("Retry-After = %q", response.Header().Get("Retry-After"))
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "accelerator_unavailable" {
		t.Fatalf("error code = %q", body.Error.Code)
	}

	livenessResponse := httptest.NewRecorder()
	handler.ServeHTTP(livenessResponse, httptest.NewRequest(http.MethodGet, "/v2/health/live", nil))
	if livenessResponse.Code != http.StatusOK {
		t.Fatalf("standby liveness status = %d", livenessResponse.Code)
	}
}

func TestStandbyIsHealthyButNotReady(t *testing.T) {
	handler := newTestHandler(t, NewUnavailableHandler())

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health status = %d", health.Code)
	}

	readiness := httptest.NewRecorder()
	handler.ServeHTTP(readiness, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if readiness.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness status = %d", readiness.Code)
	}
}

func TestRootAndUnknownPathsDoNotReachBackend(t *testing.T) {
	handler := newTestHandler(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("backend should not be called")
	}))

	root := httptest.NewRecorder()
	handler.ServeHTTP(root, httptest.NewRequest(http.MethodGet, "/", nil))
	if root.Code != http.StatusOK {
		t.Fatalf("root status = %d", root.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(root.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "piccolo-chat" {
		t.Fatalf("root model = %q", body["model"])
	}

	unknown := httptest.NewRecorder()
	handler.ServeHTTP(unknown, httptest.NewRequest(http.MethodGet, "/admin", nil))
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown status = %d", unknown.Code)
	}
}

func TestV3RejectsDotSegmentsBeforeProxying(t *testing.T) {
	handler := newTestHandler(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("backend should not receive a traversal-shaped path")
	}))

	for _, path := range []string{
		"/v3/../v2/health/ready",
		"/v3/./models",
		"/v3/%2e%2e/v2/health/ready",
		"/v3/%2E%2E/v2/health/ready",
		"/v3/%2e./v2/health/ready",
	} {
		t.Run(path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.Header.Set("Authorization", "Bearer "+testToken)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", response.Code, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), "invalid_path") {
				t.Fatalf("unexpected error body: %s", response.Body.String())
			}
		})
	}
}

func TestClientCancellationReachesBackend(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	target, err := url.Parse("http://127.0.0.1:8001")
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(Config{APIToken: testToken, Backend: target, ModelName: "piccolo-chat", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	handler.proxy.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		close(cancelled)
		return nil, r.Context().Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/v3/chat/completions", strings.NewReader(`{}`)).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+testToken)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(response, request)
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("backend request did not start")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("backend did not observe client cancellation")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("gateway did not finish cancelled request")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestBackendFailureUsesStableServiceUnavailableShape(t *testing.T) {
	target, err := url.Parse("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(Config{APIToken: testToken, Backend: target, ModelName: "piccolo-chat", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/v3/models", nil)
	request.Header.Set("Authorization", "Bearer "+testToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "backend_unavailable") {
		t.Fatalf("unexpected error body: %s", response.Body.String())
	}
}

func TestV3EnforcesKnownAndStreamingBodyLimits(t *testing.T) {
	backendCalls := 0
	handler := newTestHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls++
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	handler.limits.MaxRequestBytes = 4

	known := httptest.NewRequest(http.MethodPost, "/v3/chat/completions", strings.NewReader("12345"))
	known.Header.Set("Authorization", "Bearer "+testToken)
	knownResponse := httptest.NewRecorder()
	handler.ServeHTTP(knownResponse, known)
	if knownResponse.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("known-length status = %d, want 413; body=%s", knownResponse.Code, knownResponse.Body.String())
	}
	if backendCalls != 0 {
		t.Fatalf("backend calls = %d, want 0", backendCalls)
	}

	handler.proxy.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		_, err := io.Copy(io.Discard, r.Body)
		return nil, err
	})
	streaming := httptest.NewRequest(http.MethodPost, "/v3/chat/completions", strings.NewReader("12345"))
	streaming.ContentLength = -1
	streaming.Header.Set("Authorization", "Bearer "+testToken)
	streamingResponse := httptest.NewRecorder()
	handler.ServeHTTP(streamingResponse, streaming)
	if streamingResponse.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("streaming status = %d, want 413; body=%s", streamingResponse.Code, streamingResponse.Body.String())
	}
}

func TestV3BoundsConcurrentRequests(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	handler := newTestHandler(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	handler.requestSlots = make(chan struct{}, 1)

	firstRequest := httptest.NewRequest(http.MethodPost, "/v3/chat/completions", strings.NewReader(`{}`))
	firstRequest.Header.Set("Authorization", "Bearer "+testToken)
	firstResponse := httptest.NewRecorder()
	firstDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(firstResponse, firstRequest)
		close(firstDone)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first request did not reach backend")
	}
	secondRequest := httptest.NewRequest(http.MethodPost, "/v3/chat/completions", strings.NewReader(`{}`))
	secondRequest.Header.Set("Authorization", "Bearer "+testToken)
	secondResponse := httptest.NewRecorder()
	handler.ServeHTTP(secondResponse, secondRequest)
	if secondResponse.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want 429; body=%s", secondResponse.Code, secondResponse.Body.String())
	}
	if secondResponse.Header().Get("Retry-After") != "1" {
		t.Fatalf("Retry-After = %q, want 1", secondResponse.Header().Get("Retry-After"))
	}
	close(release)
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first request did not finish")
	}
}

func TestV3BoundsRequestDuration(t *testing.T) {
	target, err := url.Parse("http://127.0.0.1:8001")
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(Config{
		APIToken:  testToken,
		Backend:   target,
		ModelName: "piccolo-chat",
		Version:   "test",
		Limits: Limits{
			MaxRequestBytes:       1024,
			MaxConcurrentRequests: 1,
			MaxRequestDuration:    20 * time.Millisecond,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler.proxy.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})

	request := httptest.NewRequest(http.MethodPost, "/v3/chat/completions", strings.NewReader(`{}`))
	request.Header.Set("Authorization", "Bearer "+testToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504; body=%s", response.Code, response.Body.String())
	}
}

func TestServerReturnsStructuredTimeout(t *testing.T) {
	target, err := url.Parse("http://127.0.0.1:8001")
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(Config{
		APIToken:  testToken,
		Backend:   target,
		ModelName: "piccolo-chat",
		Version:   "test",
		Limits: Limits{
			MaxRequestBytes:          1024,
			MaxConcurrentRequests:    1,
			MaxRequestDuration:       30 * time.Millisecond,
			MaxRequestUploadDuration: time.Second,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler.proxy.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(listener.Addr().String(), handler, time.Second, 30*time.Millisecond)
	t.Cleanup(func() { _ = server.Close() })
	go func() { _ = server.Serve(listener) }()

	request, err := http.NewRequest(
		http.MethodPost,
		"http://"+listener.Addr().String()+"/v3/chat/completions",
		strings.NewReader(`{}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	response, err := (&http.Client{Timeout: 2 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504; body=%s", response.StatusCode, body)
	}
	if !strings.Contains(string(body), `"code":"request_timeout"`) {
		t.Fatalf("unexpected timeout body: %s", body)
	}
}

func TestServerUploadTimeoutReleasesRequestSlot(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()
	target, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(Config{
		APIToken:  testToken,
		Backend:   target,
		ModelName: "piccolo-chat",
		Version:   "test",
		Limits: Limits{
			MaxRequestBytes:          1024,
			MaxConcurrentRequests:    1,
			MaxRequestDuration:       time.Second,
			MaxRequestUploadDuration: 100 * time.Millisecond,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(listener.Addr().String(), handler, 100*time.Millisecond, time.Second)
	t.Cleanup(func() { _ = server.Close() })
	go func() { _ = server.Serve(listener) }()

	slow, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = slow.Close() })
	_, err = fmt.Fprintf(slow, "POST /v3/chat/completions HTTP/1.1\r\nHost: test\r\nAuthorization: Bearer %s\r\nTransfer-Encoding: chunked\r\n\r\n1\r\nx\r\n", testToken)
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)
	for len(handler.requestSlots) != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(handler.requestSlots) != 1 {
		t.Fatal("slow upload did not acquire the request slot")
	}

	client := &http.Client{Timeout: time.Second}
	request, err := http.NewRequest(http.MethodPost, "http://"+listener.Addr().String()+"/v3/chat/completions", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status while slow upload holds slot = %d, want 429", response.StatusCode)
	}

	deadline = time.Now().Add(time.Second)
	for len(handler.requestSlots) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(handler.requestSlots) != 0 {
		t.Fatal("request slot was not released after upload timeout")
	}

	request, err = http.NewRequest(http.MethodPost, "http://"+listener.Addr().String()+"/v3/chat/completions", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status after upload timeout = %d, want 204", response.StatusCode)
	}
}

func TestServerWriteTimeoutReleasesRequestSlotForNonReadingClient(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("backend response writer does not support flushing")
			return
		}
		chunk := make([]byte, 64<<10)
		for {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			flusher.Flush()
			select {
			case <-r.Context().Done():
				return
			default:
			}
		}
	}))
	defer backend.Close()
	target, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(Config{
		APIToken:  testToken,
		Backend:   target,
		ModelName: "piccolo-chat",
		Version:   "test",
		Limits: Limits{
			MaxRequestBytes:          1024,
			MaxConcurrentRequests:    1,
			MaxRequestDuration:       100 * time.Millisecond,
			MaxRequestUploadDuration: time.Second,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := smallWriteBufferListener{Listener: tcpListener}
	server := NewServer(listener.Addr().String(), handler, time.Second, 100*time.Millisecond)
	t.Cleanup(func() { _ = server.Close() })
	go func() { _ = server.Serve(listener) }()

	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	_, err = fmt.Fprintf(client, "POST /v3/chat/completions HTTP/1.1\r\nHost: test\r\nAuthorization: Bearer %s\r\nContent-Length: 0\r\n\r\n", testToken)
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)
	for len(handler.requestSlots) != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(handler.requestSlots) != 1 {
		t.Fatal("non-reading client did not acquire the request slot")
	}
	deadline = time.Now().Add(2 * time.Second)
	for len(handler.requestSlots) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(handler.requestSlots) != 0 {
		t.Fatal("request slot was not released after response write timeout")
	}
}

type smallWriteBufferListener struct {
	net.Listener
}

func (l smallWriteBufferListener) Accept() (net.Conn, error) {
	connection, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if tcpConnection, ok := connection.(*net.TCPConn); ok {
		if err := tcpConnection.SetWriteBuffer(1024); err != nil {
			connection.Close()
			return nil, err
		}
	}
	return connection, nil
}

func TestNewRejectsWeakTokensAndNonHTTPBackends(t *testing.T) {
	target, _ := url.Parse("http://127.0.0.1:8001")
	if _, err := New(Config{APIToken: "short", Backend: target, ModelName: "piccolo-chat"}); err == nil {
		t.Fatal("expected weak token to fail")
	}
	target, _ = url.Parse("https://127.0.0.1:8001")
	if _, err := New(Config{APIToken: testToken, Backend: target, ModelName: "piccolo-chat"}); err == nil {
		t.Fatal("expected https backend to fail")
	}
}

func ExampleHandler() {
	fmt.Println("Piccolo AI gateway exposes /v3 with bearer authentication")
	// Output: Piccolo AI gateway exposes /v3 with bearer authentication
}
