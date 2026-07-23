package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/AtDexters-Lab/piccolo-ai/internal/backend/ovms"
	"github.com/AtDexters-Lab/piccolo-ai/internal/gateway"
	"github.com/AtDexters-Lab/piccolo-ai/internal/provider"
)

var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Printf("fatal: %v", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "version":
			fmt.Printf("piccolo-ai-ovms %s (%s)\n", version, commit)
			return nil
		case "healthcheck":
			return gateway.CheckHealth("http://127.0.0.1:8000/healthz")
		case "serve":
			if len(args) > 1 {
				return fmt.Errorf("serve takes no arguments")
			}
		default:
			return fmt.Errorf("unknown command %q", args[0])
		}
	}
	apiToken := os.Getenv("PICCOLO_AI_API_TOKEN")
	limits, err := gateway.LimitsFromEnv()
	if err != nil {
		return err
	}
	backendURL, err := url.Parse("http://127.0.0.1:8001")
	if err != nil {
		return err
	}
	handler, err := gateway.New(gateway.Config{
		APIToken:  apiToken,
		Backend:   backendURL,
		ModelName: ovms.ModelName,
		Version:   version,
		Limits:    limits,
	})
	if err != nil {
		return err
	}

	backendConfig, err := ovms.ConfigFromEnv()
	if err != nil {
		return err
	}
	if err := backendConfig.Prepare(); err != nil {
		return err
	}
	command := backendConfig.Command()

	listener, err := net.Listen("tcp", "0.0.0.0:8000")
	if err != nil {
		return fmt.Errorf("listen on gateway port: %w", err)
	}
	server := gateway.NewServer("0.0.0.0:8000", handler, limits.MaxRequestUploadDuration, limits.MaxRequestDuration)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Printf("starting Piccolo AI %s (%s), model=%s target_device=%s", version, commit, ovms.ModelName, backendConfig.TargetDevice)
	return provider.Run(ctx, command, listener, server, 8*time.Second)
}
