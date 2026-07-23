package provider

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestRunReturnsWhenBackendExits(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})}
	command := exec.Command("/bin/sh", "-c", "exit 7")
	err = Run(context.Background(), command, listener, server, time.Second)
	if err == nil {
		t.Fatal("expected backend exit to fail the provider")
	}
}

func TestRunStopsBackendOnContextCancellation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})}
	started := filepath.Join(t.TempDir(), "started")
	command := exec.Command("/bin/sh", "-c", "printf started > \"$1\"; trap 'exit 0' TERM; while :; do sleep 1; done", "provider-test", started)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, command, listener, server, 2*time.Second) }()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(started); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(started); err != nil {
		t.Fatal("backend did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() after cancellation = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("provider did not stop after cancellation")
	}
}

func TestRunSharesOneShutdownBudgetBetweenGatewayAndBackend(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	requestStarted := make(chan struct{})
	server := &http.Server{Handler: http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(requestStarted)
		<-request.Context().Done()
	})}
	backendStarted := filepath.Join(t.TempDir(), "backend-started")
	command := exec.Command("/bin/sh", "-c", "printf started > \"$1\"; trap '' TERM; while :; do sleep 1; done", "provider-budget-test", backendStarted)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	const shutdownBudget = 400 * time.Millisecond
	go func() { done <- Run(ctx, command, listener, server, shutdownBudget) }()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(backendStarted); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(backendStarted); err != nil {
		t.Fatal("backend did not start")
	}
	go func() {
		_, _ = http.Get("http://" + listener.Addr().String())
	}()
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("gateway request did not start")
	}

	started := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() after cancellation = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("provider exceeded its overall shutdown budget")
	}
	if elapsed := time.Since(started); elapsed >= 650*time.Millisecond {
		t.Fatalf("shutdown took %s, want one %s overall budget", elapsed, shutdownBudget)
	}
}

func TestRunStandbyServesBothListenersUntilCancellation(t *testing.T) {
	gatewayListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	capabilityListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		gatewayListener.Close()
		t.Fatal(err)
	}
	gatewayServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})}
	capabilityServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunStandby(ctx, gatewayListener, gatewayServer, capabilityListener, capabilityServer, time.Second)
	}()

	waitForStatus(t, "http://"+gatewayListener.Addr().String(), http.StatusOK)
	waitForStatus(t, "http://"+capabilityListener.Addr().String(), http.StatusServiceUnavailable)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunStandby() after cancellation = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("standby provider did not stop after cancellation")
	}
}

func waitForStatus(t *testing.T, endpoint string, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get(endpoint)
		if err == nil {
			response.Body.Close()
			if response.StatusCode == want {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s did not return %d", endpoint, want)
}
