package ovms

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
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
	CacheDir     string
	TargetDevice string
	LogLevel     string
}

func ConfigFromEnv() (Config, error) {
	cfg := Config{
		Binary:       envOrDefault("PICCOLO_AI_OVMS_BINARY", "/ovms/bin/ovms"),
		ModelPath:    envOrDefault("PICCOLO_AI_MODEL_PATH", "/models/model"),
		CacheDir:     envOrDefault("PICCOLO_AI_CACHE_DIR", "/var/cache/ovms"),
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
	if !filepath.IsAbs(cfg.Binary) || !filepath.IsAbs(cfg.ModelPath) || !filepath.IsAbs(cfg.CacheDir) {
		return Config{}, errors.New("OVMS binary, model path, and cache directory must be absolute paths")
	}
	return cfg, nil
}

// Prepare validates the read-only model mount and proves that the configured
// cache directory is writable before the long-lived server is started.
func (c Config) Prepare() error {
	info, err := os.Stat(c.ModelPath)
	if err != nil {
		return fmt.Errorf("model path %q: %w", c.ModelPath, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("model path %q is not a directory", c.ModelPath)
	}

	if err := os.MkdirAll(c.CacheDir, 0o700); err != nil {
		return fmt.Errorf("create cache directory %q: %w", c.CacheDir, err)
	}
	probe, err := os.CreateTemp(c.CacheDir, ".piccolo-write-check-")
	if err != nil {
		return fmt.Errorf("cache directory %q is not writable: %w", c.CacheDir, err)
	}
	probeName := probe.Name()
	if err := probe.Close(); err != nil {
		return fmt.Errorf("close cache write check: %w", err)
	}
	if err := os.Remove(probeName); err != nil {
		return fmt.Errorf("remove cache write check: %w", err)
	}
	return nil
}

func (c Config) Args() []string {
	return []string{
		"--model_path", c.ModelPath,
		"--model_name", ModelName,
		"--rest_port", fmt.Sprintf("%d", BackendPort),
		"--rest_bind_address", "0.0.0.0",
		"--task", "text_generation",
		"--target_device", c.TargetDevice,
		"--cache_dir", c.CacheDir,
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
