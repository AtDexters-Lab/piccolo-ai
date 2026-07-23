package gateway

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultMaxRequestBytes          = int64(256 << 20)
	defaultMaxConcurrentRequests    = 32
	defaultMaxRequestDuration       = 30 * time.Minute
	defaultMaxRequestUploadDuration = 5 * time.Minute
)

type Limits struct {
	MaxRequestBytes          int64
	MaxConcurrentRequests    int
	MaxRequestDuration       time.Duration
	MaxRequestUploadDuration time.Duration
}

func LimitsFromEnv() (Limits, error) {
	maxRequestBytes, err := positiveInt64Env("PICCOLO_AI_MAX_REQUEST_BYTES", defaultMaxRequestBytes)
	if err != nil {
		return Limits{}, err
	}
	maxConcurrentRequests, err := positiveInt64Env("PICCOLO_AI_MAX_CONCURRENT_REQUESTS", defaultMaxConcurrentRequests)
	if err != nil {
		return Limits{}, err
	}
	if int64(int(maxConcurrentRequests)) != maxConcurrentRequests {
		return Limits{}, fmt.Errorf("PICCOLO_AI_MAX_CONCURRENT_REQUESTS %d exceeds this platform's integer range", maxConcurrentRequests)
	}
	maxRequestDuration := defaultMaxRequestDuration
	if value := strings.TrimSpace(os.Getenv("PICCOLO_AI_MAX_REQUEST_DURATION")); value != "" {
		maxRequestDuration, err = time.ParseDuration(value)
		if err != nil || maxRequestDuration <= 0 {
			return Limits{}, fmt.Errorf("PICCOLO_AI_MAX_REQUEST_DURATION %q must be a positive Go duration", value)
		}
	}
	maxRequestUploadDuration := defaultMaxRequestUploadDuration
	if value := strings.TrimSpace(os.Getenv("PICCOLO_AI_MAX_REQUEST_UPLOAD_DURATION")); value != "" {
		maxRequestUploadDuration, err = time.ParseDuration(value)
		if err != nil || maxRequestUploadDuration <= 0 {
			return Limits{}, fmt.Errorf("PICCOLO_AI_MAX_REQUEST_UPLOAD_DURATION %q must be a positive Go duration", value)
		}
	}
	return Limits{
		MaxRequestBytes:          maxRequestBytes,
		MaxConcurrentRequests:    int(maxConcurrentRequests),
		MaxRequestDuration:       maxRequestDuration,
		MaxRequestUploadDuration: maxRequestUploadDuration,
	}, nil
}

func normalizeLimits(limits Limits) (Limits, error) {
	if limits.MaxRequestBytes == 0 {
		limits.MaxRequestBytes = defaultMaxRequestBytes
	}
	if limits.MaxConcurrentRequests == 0 {
		limits.MaxConcurrentRequests = defaultMaxConcurrentRequests
	}
	if limits.MaxRequestDuration == 0 {
		limits.MaxRequestDuration = defaultMaxRequestDuration
	}
	if limits.MaxRequestUploadDuration == 0 {
		limits.MaxRequestUploadDuration = defaultMaxRequestUploadDuration
	}
	if limits.MaxRequestBytes < 0 || limits.MaxConcurrentRequests < 0 || limits.MaxRequestDuration < 0 || limits.MaxRequestUploadDuration < 0 {
		return Limits{}, fmt.Errorf("gateway request limits must be positive")
	}
	return limits, nil
}

func positiveInt64Env(name string, fallback int64) (int64, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s %q must be a positive integer", name, value)
	}
	return parsed, nil
}
