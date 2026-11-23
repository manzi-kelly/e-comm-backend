//go:build !js

package tests

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"autivashop/backend/internal/server"
)

// TestRunServesAndShutsDown verifies the real TCP lifecycle: Run listens on
// the configured address, serves health, and returns cleanly on cancellation.
func TestRunServesAndShutsDown(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("could not reserve a loopback port: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()

	cfg := testConfig(t, map[string]string{
		"API_HOST": "127.0.0.1",
		"API_PORT": strconv.Itoa(port),
	})
	srv := server.New(cfg, stubPinger{}, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	url := fmt.Sprintf("http://127.0.0.1:%d/api/v1/health", port)
	var resp *http.Response
	for i := 0; i < 100; i++ {
		resp, err = http.Get(url)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		cancel()
		<-done
		t.Fatalf("server did not accept connections: %v", err)
	}
	status := resp.StatusCode
	_ = resp.Body.Close()
	if status != http.StatusOK {
		cancel()
		<-done
		t.Fatalf("health status = %d, want 200", status)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() returned %v on graceful shutdown", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run() did not shut down within the timeout")
	}
}
