package ovms

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	ModelName   = "piccolo-chat"
	BackendPort = 8001
)

var targetDevicePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_.:-]*(,[A-Z0-9_.:-]+)*$`)

// Config is the deliberately small Piccolo-facing configuration surface for
// the OVMS backend. Model identity and ports are fixed by the provider contract.
type Config struct {
	Binary       string
	ModelPath    string
	TargetDevice string
	LogLevel     string
}

func ConfigFromEnv() (Config, error) {
	cfg := Config{
		Binary:       envOrDefault("PICCOLO_AI_OVMS_BINARY", "/ovms/bin/ovms"),
		ModelPath:    envOrDefault("PICCOLO_AI_MODEL_PATH", "/models/model"),
		TargetDevice: strings.ToUpper(envOrDefault("PICCOLO_AI_TARGET_DEVICE", "AUTO:GPU,CPU")),
		LogLevel:     strings.ToUpper(envOrDefault("PICCOLO_AI_LOG_LEVEL", "INFO")),
	}

	if !targetDevicePattern.MatchString(cfg.TargetDevice) {
		return Config{}, fmt.Errorf("PICCOLO_AI_TARGET_DEVICE %q is invalid", cfg.TargetDevice)
	}
	switch cfg.LogLevel {
	case "DEBUG", "INFO", "ERROR":
	default:
		return Config{}, fmt.Errorf("PICCOLO_AI_LOG_LEVEL %q must be DEBUG, INFO, or ERROR", cfg.LogLevel)
	}
	if !filepath.IsAbs(cfg.Binary) || !filepath.IsAbs(cfg.ModelPath) {
		return Config{}, errors.New("OVMS binary and model path must be absolute paths")
	}
	return cfg, nil
}

// Prepare validates the read-only model mount before the long-lived server is
// started.
func (c Config) Prepare() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, c.Binary, "--version").Run(); err != nil {
		return fmt.Errorf("validate OVMS binary %q: %w", c.Binary, err)
	}
	info, err := os.Stat(c.ModelPath)
	if err != nil {
		return fmt.Errorf("model path %q: %w", c.ModelPath, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("model path %q is not a directory", c.ModelPath)
	}
	entries, err := os.ReadDir(c.ModelPath)
	if err != nil {
		return fmt.Errorf("read model path %q: %w", c.ModelPath, err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("model path %q is empty", c.ModelPath)
	}
	return nil
}

// UnavailableRequiredAccelerator reports an accelerator that the configured
// target strictly requires but that Piccolod has not mapped into the container.
// CPU-fallback AUTO targets can bootstrap without capability-derived devices.
func (c Config) UnavailableRequiredAccelerator() string {
	return unavailableRequiredAccelerator(c.TargetDevice, hasGPUDevice(), hasNPUDevice())
}

func unavailableRequiredAccelerator(target string, gpuAvailable, npuAvailable bool) string {
	parts := strings.SplitN(strings.ToUpper(target), ":", 2)
	if len(parts) == 1 {
		switch {
		case parts[0] == "GPU" || strings.HasPrefix(parts[0], "GPU."):
			if !gpuAvailable {
				return "GPU"
			}
		case parts[0] == "NPU" || strings.HasPrefix(parts[0], "NPU."):
			if !npuAvailable {
				return "NPU"
			}
		}
		return ""
	}

	mode := parts[0]
	if mode != "AUTO" && mode != "MULTI" && mode != "HETERO" {
		return ""
	}
	devices := strings.Split(parts[1], ",")
	if mode == "AUTO" {
		for _, device := range devices {
			if device == "CPU" {
				return ""
			}
			if (device == "GPU" || strings.HasPrefix(device, "GPU.")) && gpuAvailable {
				return ""
			}
			if (device == "NPU" || strings.HasPrefix(device, "NPU.")) && npuAvailable {
				return ""
			}
		}
		for _, device := range devices {
			switch {
			case device == "GPU" || strings.HasPrefix(device, "GPU."):
				return "GPU"
			case device == "NPU" || strings.HasPrefix(device, "NPU."):
				return "NPU"
			}
		}
		return ""
	}

	for _, device := range devices {
		switch {
		case (device == "GPU" || strings.HasPrefix(device, "GPU.")) && !gpuAvailable:
			return "GPU"
		case (device == "NPU" || strings.HasPrefix(device, "NPU.")) && !npuAvailable:
			return "NPU"
		}
	}
	return ""
}

func hasGPUDevice() bool {
	return hasUsableDevice("/dev/dri/renderD*")
}

func hasNPUDevice() bool {
	return hasUsableDevice("/dev/accel/accel*")
}

func hasUsableDevice(pattern string) bool {
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return false
	}
	for _, path := range matches {
		device, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			continue
		}
		device.Close()
		return true
	}
	return false
}

func (c Config) Args() []string {
	return []string{
		"--model_path", c.ModelPath,
		"--model_name", ModelName,
		"--rest_port", fmt.Sprintf("%d", BackendPort),
		"--rest_bind_address", "0.0.0.0",
		"--task", "text_generation",
		"--target_device", c.TargetDevice,
		"--file_system_poll_wait_seconds", "0",
		"--log_level", c.LogLevel,
	}
}

func (c Config) Command() *exec.Cmd {
	return exec.Command(c.Binary, c.Args()...)
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
