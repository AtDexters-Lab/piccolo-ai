package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

const minimumTokenLength = 24

type Config struct {
	APIToken  string
	Backend   *url.URL
	ModelName string
	Version   string
	Limits    Limits
}

type Handler struct {
	token         string
	backend       *url.URL
	modelName     string
	version       string
	proxy         *httputil.ReverseProxy
	backendClient *http.Client
	limits        Limits
	requestSlots  chan struct{}
}

func New(config Config) (*Handler, error) {
	if len(config.APIToken) < minimumTokenLength || strings.TrimSpace(config.APIToken) != config.APIToken {
		return nil, fmt.Errorf("PICCOLO_AI_API_TOKEN must contain at least %d non-whitespace characters", minimumTokenLength)
	}
	if config.Backend == nil || config.Backend.Scheme != "http" || config.Backend.Host == "" {
		return nil, errors.New("backend must be an absolute http URL")
	}
	if config.ModelName == "" {
		return nil, errors.New("model name is required")
	}
	limits, err := normalizeLimits(config.Limits)
	if err != nil {
		return nil, err
	}

	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: time.Second,
	}

	h := &Handler{
		token:        config.APIToken,
		backend:      cloneURL(config.Backend),
		modelName:    config.ModelName,
		version:      config.Version,
		limits:       limits,
		requestSlots: make(chan struct{}, limits.MaxConcurrentRequests),
		backendClient: &http.Client{
			Transport: transport,
			Timeout:   2 * time.Second,
		},
	}
	h.proxy = &httputil.ReverseProxy{
		Rewrite:       h.rewrite,
		Transport:     transport,
		FlushInterval: -1,
		ErrorHandler:  h.proxyError,
	}
	return h, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/healthz":
		h.serveHealth(w, r)
	case r.URL.Path == "/readyz":
		h.serveReadiness(w, r)
	case r.URL.Path == "/":
		h.serveRoot(w, r)
	case r.URL.Path == "/v3" || strings.HasPrefix(r.URL.Path, "/v3/"):
		h.serveV3(w, r)
	default:
		writeOpenAIError(w, http.StatusNotFound, "Not found", "invalid_request_error", "not_found")
	}
}

func (h *Handler) serveV3(w http.ResponseWriter, r *http.Request) {
	if containsDotSegment(r.URL.Path) {
		writeOpenAIError(w, http.StatusBadRequest, "Invalid API path", "invalid_request_error", "invalid_path")
		return
	}
	if !h.authorized(r.Header.Values("Authorization")) {
		writeOpenAIError(w, http.StatusUnauthorized, "Missing or invalid API token", "authentication_error", "invalid_api_key")
		return
	}
	if r.ContentLength > h.limits.MaxRequestBytes {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "Request body too large", "invalid_request_error", "request_too_large")
		return
	}

	select {
	case h.requestSlots <- struct{}{}:
		defer func() { <-h.requestSlots }()
	default:
		w.Header().Set("Retry-After", "1")
		writeOpenAIError(w, http.StatusTooManyRequests, "Too many concurrent inference requests", "rate_limit_error", "concurrency_limit_exceeded")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, h.limits.MaxRequestBytes)
	ctx, cancel := context.WithTimeout(r.Context(), h.limits.MaxRequestDuration)
	defer cancel()
	h.proxy.ServeHTTP(w, r.WithContext(ctx))
}

func (h *Handler) authorized(values []string) bool {
	if len(values) != 1 {
		return false
	}
	const prefix = "Bearer "
	value := values[0]
	if len(value) <= len(prefix) || !strings.EqualFold(value[:len(prefix)], prefix) {
		return false
	}
	presented := value[len(prefix):]
	return len(presented) == len(h.token) && subtle.ConstantTimeCompare([]byte(presented), []byte(h.token)) == 1
}

func (h *Handler) rewrite(request *httputil.ProxyRequest) {
	request.SetURL(h.backend)
	request.Out.Host = h.backend.Host
	for name := range request.Out.Header {
		if shouldStripProxyHeader(name) {
			request.Out.Header.Del(name)
		}
	}
}

func shouldStripProxyHeader(name string) bool {
	lowerName := strings.ToLower(name)
	return lowerName == "authorization" ||
		lowerName == "cookie" ||
		lowerName == "forwarded" ||
		lowerName == "x-real-ip" ||
		strings.HasPrefix(lowerName, "x-forwarded-") ||
		strings.HasPrefix(lowerName, "x-piccolo-")
}

func (h *Handler) proxyError(w http.ResponseWriter, request *http.Request, err error) {
	log.Printf("inference proxy error: method=%s path=%q: %v", request.Method, request.URL.EscapedPath(), err)
	var maxBytesError *http.MaxBytesError
	if errors.As(err, &maxBytesError) {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "Request body too large", "invalid_request_error", "request_too_large")
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		writeOpenAIError(w, http.StatusGatewayTimeout, "Inference request timed out", "server_error", "request_timeout")
		return
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "Inference backend unavailable", "server_error", "backend_unavailable")
}

func containsDotSegment(path string) bool {
	for _, segment := range strings.Split(path, "/") {
		if segment == "." || segment == ".." {
			return true
		}
	}
	return false
}

func (h *Handler) serveHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}
	status, healthy := h.intrinsicHealth(r.Context())
	if !healthy {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": status})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": status})
}

func (h *Handler) serveReadiness(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}
	readinessURL := h.backend.ResolveReference(&url.URL{Path: "/v2/models/" + h.modelName + "/ready"})
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, readinessURL.String(), nil)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	response, err := h.backendClient.Do(request)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (h *Handler) intrinsicHealth(ctx context.Context) (string, bool) {
	livenessURL := h.backend.ResolveReference(&url.URL{Path: "/v2/health/live"})
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, livenessURL.String(), nil)
	if err != nil {
		return "unhealthy", false
	}
	response, err := h.backendClient.Do(request)
	if err != nil {
		return "unhealthy", false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "unhealthy", false
	}
	return "healthy", true
}

// NewUnavailableHandler exposes the capability listener while an external
// accelerator grant is unavailable. It deliberately has no provider
// credential because Piccolod owns access to the private capability route.
func NewUnavailableHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && r.URL.Path == "/v2/health/live" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Retry-After", "5")
		writeOpenAIError(w, http.StatusServiceUnavailable, "Inference accelerator unavailable", "server_error", "accelerator_unavailable")
	})
}

func (h *Handler) serveRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"name":    "Piccolo AI",
		"model":   h.modelName,
		"version": h.version,
	})
}

func NewServer(address string, handler http.Handler, requestUploadTimeout, requestTimeout time.Duration) *http.Server {
	if requestUploadTimeout <= 0 {
		requestUploadTimeout = defaultMaxRequestUploadDuration
	}
	if requestTimeout <= 0 {
		requestTimeout = defaultMaxRequestDuration
	}
	const timeoutResponseGrace = time.Second
	const maxDuration = time.Duration(1<<63 - 1)
	writeTimeout := requestTimeout
	if requestTimeout <= maxDuration-timeoutResponseGrace {
		writeTimeout += timeoutResponseGrace
	}
	return &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       requestUploadTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
}

func CheckHealth(endpoint string) error {
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get(endpoint)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("health endpoint returned %s", response.Status)
	}
	return nil
}

func cloneURL(value *url.URL) *url.URL {
	clone := *value
	return &clone
}

func writeOpenAIError(w http.ResponseWriter, status int, message, kind, code string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    kind,
			"param":   nil,
			"code":    code,
		},
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
