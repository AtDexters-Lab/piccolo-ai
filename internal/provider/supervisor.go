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
