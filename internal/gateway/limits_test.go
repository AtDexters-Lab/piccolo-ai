package gateway

import (
	"testing"
	"time"
)

func TestLimitsFromEnvDefaultsAndOverrides(t *testing.T) {
	for _, name := range []string{
		"PICCOLO_AI_MAX_REQUEST_BYTES",
		"PICCOLO_AI_MAX_CONCURRENT_REQUESTS",
		"PICCOLO_AI_MAX_REQUEST_DURATION",
		"PICCOLO_AI_MAX_REQUEST_UPLOAD_DURATION",
	} {
		t.Setenv(name, "")
	}
	limits, err := LimitsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if limits.MaxRequestBytes != defaultMaxRequestBytes || limits.MaxConcurrentRequests != defaultMaxConcurrentRequests || limits.MaxRequestDuration != defaultMaxRequestDuration || limits.MaxRequestUploadDuration != defaultMaxRequestUploadDuration {
		t.Fatalf("unexpected defaults: %#v", limits)
	}

	t.Setenv("PICCOLO_AI_MAX_REQUEST_BYTES", "1024")
	t.Setenv("PICCOLO_AI_MAX_CONCURRENT_REQUESTS", "7")
	t.Setenv("PICCOLO_AI_MAX_REQUEST_DURATION", "45s")
	t.Setenv("PICCOLO_AI_MAX_REQUEST_UPLOAD_DURATION", "15s")
	limits, err = LimitsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if limits.MaxRequestBytes != 1024 || limits.MaxConcurrentRequests != 7 || limits.MaxRequestDuration != 45*time.Second || limits.MaxRequestUploadDuration != 15*time.Second {
		t.Fatalf("unexpected overrides: %#v", limits)
	}
}

func TestLimitsFromEnvRejectsInvalidValues(t *testing.T) {
	for _, test := range []struct {
		name  string
		value string
	}{
		{name: "PICCOLO_AI_MAX_REQUEST_BYTES", value: "0"},
		{name: "PICCOLO_AI_MAX_CONCURRENT_REQUESTS", value: "many"},
		{name: "PICCOLO_AI_MAX_REQUEST_DURATION", value: "forever"},
		{name: "PICCOLO_AI_MAX_REQUEST_UPLOAD_DURATION", value: "0s"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(test.name, test.value)
			if _, err := LimitsFromEnv(); err == nil {
				t.Fatal("expected invalid limit to fail")
			}
		})
	}
}
