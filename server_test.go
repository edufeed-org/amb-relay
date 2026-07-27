package main

import (
	"context"
	"net/http"
	"os"
	"syscall"
	"testing"
	"time"
)

// fakeGracefulServer is a minimal gracefulServer whose ListenAndServe blocks
// until either Shutdown is called or the test closes stopCh directly (to
// simulate an unprompted listener exit, e.g. a bind error).
type fakeGracefulServer struct {
	stopCh         chan struct{}
	shutdownCalled chan struct{}
	shutdownErr    error
	listenErr      error
}

func newFakeGracefulServer() *fakeGracefulServer {
	return &fakeGracefulServer{
		stopCh:         make(chan struct{}),
		shutdownCalled: make(chan struct{}),
	}
}

func (f *fakeGracefulServer) ListenAndServe() error {
	<-f.stopCh
	if f.listenErr != nil {
		return f.listenErr
	}
	return http.ErrServerClosed
}

func (f *fakeGracefulServer) Shutdown(ctx context.Context) error {
	close(f.shutdownCalled)
	close(f.stopCh)
	return f.shutdownErr
}

// TestRunServer_SignalTriggersShutdown verifies the core graceful-shutdown
// property: a signal on sigCh must lead to Shutdown being called and
// runServer returning, instead of the process just dying (the 2026-07-28
// root-cause bug — bare http.ListenAndServe never returned on SIGTERM, so
// main()'s deferred buffer drains never ran).
func TestRunServer_SignalTriggersShutdown(t *testing.T) {
	srv := newFakeGracefulServer()
	sigCh := make(chan os.Signal, 1)

	done := make(chan error, 1)
	go func() {
		done <- runServer(srv, sigCh, 2*time.Second)
	}()

	sigCh <- syscall.SIGTERM

	select {
	case <-srv.shutdownCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown was not called within timeout after signal")
	}

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runServer returned error %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runServer did not return after Shutdown completed")
	}
}

// TestRunServer_ListenErrorReturnsWithoutSignal verifies that a listener
// failure (e.g. port already in use) surfaces as an error from runServer
// without requiring a signal — main() must not hang forever.
func TestRunServer_ListenErrorReturnsWithoutSignal(t *testing.T) {
	srv := newFakeGracefulServer()
	srv.listenErr = http.ErrAbortHandler // any non-ErrServerClosed error
	sigCh := make(chan os.Signal, 1)

	done := make(chan error, 1)
	go func() {
		done <- runServer(srv, sigCh, 2*time.Second)
	}()

	// Simulate the listener exiting on its own (bind error etc.).
	close(srv.stopCh)

	select {
	case err := <-done:
		if err == nil {
			t.Error("runServer returned nil error, want the listen error surfaced")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runServer did not return after listener error")
	}
}
