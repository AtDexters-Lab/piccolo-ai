package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"syscall"
	"time"
)

type serverResult struct {
	name string
	err  error
}

// Run owns the lifetime of the HTTP gateway and its backend child process.
// A failure of either side tears down the other so the container cannot remain
// superficially healthy with a dead inference runtime.
func Run(ctx context.Context, command *exec.Cmd, listener net.Listener, server *http.Server, shutdownTimeout time.Duration) error {
	if command == nil || listener == nil || server == nil {
		return errors.New("command, listener, and server are required")
	}
	if shutdownTimeout <= 0 {
		return errors.New("shutdown timeout must be positive")
	}
	if command.Stdout == nil {
		command.Stdout = os.Stdout
	}
	if command.Stderr == nil {
		command.Stderr = os.Stderr
	}
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.Setpgid = true

	if err := command.Start(); err != nil {
		listener.Close()
		return fmt.Errorf("start inference backend: %w", err)
	}

	backendDone := make(chan error, 1)
	go func() { backendDone <- command.Wait() }()
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(listener) }()

	select {
	case err := <-backendDone:
		shutdownServer(server, shutdownTimeout)
		if err == nil {
			return errors.New("inference backend exited unexpectedly")
		}
		return fmt.Errorf("inference backend exited unexpectedly: %w", err)
	case err := <-serverDone:
		terminateProcessGroup(command, backendDone, shutdownTimeout)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("gateway server exited unexpectedly: %w", err)
	case <-ctx.Done():
		deadline := time.Now().Add(shutdownTimeout)
		shutdownServer(server, shutdownTimeout/2)
		terminateProcessGroup(command, backendDone, time.Until(deadline))
		return nil
	}
}

// RunStandby keeps both public and capability listeners available while
// Piccolod has not granted a strictly required accelerator. The capability
// server returns HTTP 503; the public server still exposes intrinsic health.
func RunStandby(
	ctx context.Context,
	gatewayListener net.Listener,
	gatewayServer *http.Server,
	capabilityListener net.Listener,
	capabilityServer *http.Server,
	shutdownTimeout time.Duration,
) error {
	if gatewayListener == nil || gatewayServer == nil || capabilityListener == nil || capabilityServer == nil {
		return errors.New("gateway and capability listeners and servers are required")
	}
	if shutdownTimeout <= 0 {
		return errors.New("shutdown timeout must be positive")
	}

	done := make(chan serverResult, 2)
	go func() {
		done <- serverResult{name: "gateway", err: gatewayServer.Serve(gatewayListener)}
	}()
	go func() {
		done <- serverResult{name: "capability", err: capabilityServer.Serve(capabilityListener)}
	}()

	select {
	case result := <-done:
		shutdownServers([]*http.Server{gatewayServer, capabilityServer}, shutdownTimeout)
		if result.err == nil {
			return fmt.Errorf("%s server exited unexpectedly", result.name)
		}
		return fmt.Errorf("%s server exited unexpectedly: %w", result.name, result.err)
	case <-ctx.Done():
		shutdownServers([]*http.Server{gatewayServer, capabilityServer}, shutdownTimeout)
		return nil
	}
}

func shutdownServer(server *http.Server, timeout time.Duration) {
	if timeout <= 0 {
		_ = server.Close()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		_ = server.Close()
	}
}

func shutdownServers(servers []*http.Server, timeout time.Duration) {
	if timeout <= 0 {
		for _, server := range servers {
			_ = server.Close()
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	done := make(chan struct{}, len(servers))
	for _, server := range servers {
		go func(server *http.Server) {
			if err := server.Shutdown(ctx); err != nil {
				_ = server.Close()
			}
			done <- struct{}{}
		}(server)
	}
	for range servers {
		<-done
	}
}

func terminateProcessGroup(command *exec.Cmd, done <-chan error, timeout time.Duration) {
	if command.Process == nil {
		return
	}
	_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
	if timeout <= 0 {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		<-done
		return
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return
	case <-timer.C:
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		<-done
	}
}
