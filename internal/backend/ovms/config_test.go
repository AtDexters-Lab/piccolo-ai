package ovms

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestConfigFromEnvDefaults(t *testing.T) {
	for _, name := range []string{
		"PICCOLO_AI_OVMS_BINARY",
		"PICCOLO_AI_MODEL_PATH",
		"PICCOLO_AI_TARGET_DEVICE",
		"PICCOLO_AI_LOG_LEVEL",
	} {
		t.Setenv(name, "")
	}

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Binary != "/ovms/bin/ovms" || cfg.ModelPath != "/models/model" {
		t.Fatalf("unexpected defaults: %#v", cfg)
	}
	if cfg.TargetDevice != "AUTO:GPU,CPU" || cfg.LogLevel != "INFO" {
		t.Fatalf("unexpected runtime defaults: %#v", cfg)
	}
}

func TestConfigFromEnvValidatesControlledValues(t *testing.T) {
	t.Setenv("PICCOLO_AI_TARGET_DEVICE", "GPU; touch /tmp/nope")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("expected invalid target device to fail")
	}

	t.Setenv("PICCOLO_AI_TARGET_DEVICE", "CPU")
	t.Setenv("PICCOLO_AI_LOG_LEVEL", "TRACE")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("expected invalid log level to fail")
	}
}

func TestPrepareRequiresDirectoryModel(t *testing.T) {
	root := t.TempDir()
	model := filepath.Join(root, "model")
	if err := os.Mkdir(model, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := Config{ModelPath: model}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}

	cfg.ModelPath = filepath.Join(root, "missing")
	if err := cfg.Prepare(); err == nil {
		t.Fatal("expected missing model path to fail")
	}
}

func TestArgsKeepStableProviderIdentity(t *testing.T) {
	cfg := Config{
		ModelPath:    "/model",
		TargetDevice: "GPU",
		LogLevel:     "ERROR",
	}
	want := []string{
		"--model_path", "/model",
		"--model_name", "piccolo-chat",
		"--rest_port", "8001",
		"--rest_bind_address", "0.0.0.0",
		"--task", "text_generation",
		"--target_device", "GPU",
		"--file_system_poll_wait_seconds", "0",
		"--log_level", "ERROR",
	}
	if got := cfg.Args(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Args() = %#v, want %#v", got, want)
	}
}
