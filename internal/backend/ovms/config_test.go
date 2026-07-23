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
	if err := os.WriteFile(filepath.Join(model, "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := Config{Binary: "/bin/true", ModelPath: model}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}

	cfg.ModelPath = filepath.Join(root, "missing")
	if err := cfg.Prepare(); err == nil {
		t.Fatal("expected missing model path to fail")
	}

	empty := filepath.Join(root, "empty")
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg.ModelPath = empty
	if err := cfg.Prepare(); err == nil {
		t.Fatal("expected empty model path to fail")
	}

	cfg.Binary = filepath.Join(root, "missing-ovms")
	cfg.ModelPath = model
	if err := cfg.Prepare(); err == nil {
		t.Fatal("expected missing OVMS binary to fail")
	}
}

func TestUnavailableRequiredAccelerator(t *testing.T) {
	tests := []struct {
		name         string
		target       string
		gpuAvailable bool
		npuAvailable bool
		want         string
	}{
		{name: "cpu", target: "CPU"},
		{name: "strict gpu absent", target: "GPU", want: "GPU"},
		{name: "strict gpu available", target: "GPU", gpuAvailable: true},
		{name: "qualified gpu absent", target: "GPU.0", want: "GPU"},
		{name: "strict npu absent", target: "NPU", want: "NPU"},
		{name: "strict npu available", target: "NPU", npuAvailable: true},
		{name: "auto cpu fallback", target: "AUTO:GPU,CPU"},
		{name: "auto available gpu", target: "AUTO:GPU,NPU", gpuAvailable: true},
		{name: "auto external devices absent", target: "AUTO:GPU,NPU", want: "GPU"},
		{name: "multi missing gpu", target: "MULTI:GPU,CPU", want: "GPU"},
		{name: "multi devices available", target: "MULTI:GPU,NPU", gpuAvailable: true, npuAvailable: true},
		{name: "hetero missing npu", target: "HETERO:GPU,NPU", gpuAvailable: true, want: "NPU"},
		{name: "unknown mode delegated to ovms", target: "SOMETHING:GPU"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := unavailableRequiredAccelerator(tt.target, tt.gpuAvailable, tt.npuAvailable); got != tt.want {
				t.Fatalf("unavailableRequiredAccelerator() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHasUsableDevice(t *testing.T) {
	root := t.TempDir()
	device := filepath.Join(root, "renderD128")
	if err := os.WriteFile(device, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !hasUsableDevice(filepath.Join(root, "renderD*")) {
		t.Fatal("expected writable matching device")
	}
	if hasUsableDevice(filepath.Join(root, "accel*")) {
		t.Fatal("unexpected match for absent device")
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
