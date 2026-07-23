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

func TestHealthReflectsBackendReadiness(t *testing.T) {
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

	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
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
