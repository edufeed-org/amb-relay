package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"time"
)

// gracefulServer is the subset of *http.Server that runServer needs. Tests
// inject a fake implementation so the signal → Shutdown path is exercised
// without binding a real listener.
type gracefulServer interface {
	ListenAndServe() error
	Shutdown(ctx context.Context) error
}

// runServer starts srv and blocks until either it exits on its own (e.g. a
// listen error) or a shutdown signal arrives on sigCh, in which case it calls
// srv.Shutdown with the given timeout and returns once that completes (or the
// timeout elapses).
//
// This is the fix for the 2026-07-28 root-cause: main() used to call bare
// http.ListenAndServe, which never returns on SIGTERM (the process is killed
// outright), so the deferred tsBuf.Close()/boltBuf.Close() drains in main()
// never ran and any events still sitting in the in-memory TSWriteBuffer queue
// were lost. runServer returning lets main() reach its deferred drains.
func runServer(srv gracefulServer, sigCh <-chan os.Signal, shutdownTimeout time.Duration) error {
	errCh := make(chan error, 1)
	go func() {
		err := srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-sigCh:
		log.Printf("shutdown signal received, draining write buffers (up to %s)...", shutdownTimeout)
	}

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("server shutdown: %v", err)
	}
	// Wait for ListenAndServe's goroutine to actually return so callers can
	// rely on runServer's return implying the listener is fully stopped.
	<-errCh
	return nil
}
