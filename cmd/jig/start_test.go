package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLocalCodexReady(t *testing.T) {
	for _, test := range []struct {
		name, body string
		ready, bad bool
	}{
		{"ready", `{"workers":[{"live":true,"runtimes":[{"name":"codex"}]}]}`, true, false},
		{"stale", `{"workers":[{"live":false,"runtimes":[{"name":"codex"}]}]}`, false, false},
		{"other runtime", `{"workers":[{"live":true,"runtimes":[{"name":"claude-code"}]}]}`, false, false},
		{"malformed", `<html>another application</html>`, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/workers" {
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				io.WriteString(w, test.body)
			}))
			defer server.Close()
			running, ready, err := localCodexReady(context.Background(), server.Client(), server.URL, time.Time{})
			if !running || ready != test.ready || (err != nil) != test.bad {
				t.Fatalf("running=%t ready=%t err=%v", running, ready, err)
			}
			if test.ready {
				_, ready, _ := localCodexReady(context.Background(), server.Client(), server.URL, time.Now())
				if ready {
					t.Fatal("old worker heartbeat must not count after server restart")
				}
			}
		})
	}
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	if running, _, err := localCodexReady(context.Background(), server.Client(), server.URL, time.Time{}); running || err != nil {
		t.Fatalf("stopped server: running=%t err=%v", running, err)
	}
}
