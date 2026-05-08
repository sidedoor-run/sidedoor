package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func portFromURL(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func TestProbeLocalHTTPResponding(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	state, msg, latency := probeLocalHTTP(portFromURL(t, server.URL), time.Second)
	if state != "responding" {
		t.Fatalf("expected responding, got state=%q msg=%q", state, msg)
	}
	if latency < 0 {
		t.Fatalf("expected non-negative latency, got %d", latency)
	}
}

func TestProbeLocalHTTPHeaderTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	accepted := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		close(accepted)
		time.Sleep(500 * time.Millisecond)
	}()

	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	state, msg, _ := probeLocalHTTP(port, 100*time.Millisecond)
	if state != "not_responding" {
		t.Fatalf("expected not_responding, got state=%q msg=%q", state, msg)
	}
	if !strings.Contains(msg, "did not send HTTP response headers") {
		t.Fatalf("unexpected timeout message: %q", msg)
	}

	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("local server did not accept probe connection")
	}
}
