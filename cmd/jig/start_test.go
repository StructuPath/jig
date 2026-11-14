package main

import (
	"context"
	"flag"
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
			running, ready, err := localWorkerReady(context.Background(), server.Client(), server.URL, "codex", time.Time{}, false)
			if !running || ready != test.ready || (err != nil) != test.bad {
				t.Fatalf("running=%t ready=%t err=%v", running, ready, err)
			}
			if test.ready {
				_, ready, _ := localWorkerReady(context.Background(), server.Client(), server.URL, "codex", time.Time{}, true)
				if ready {
					t.Fatal("worker without merge consent must not satisfy --allow-merge")
				}
				_, ready, _ = localWorkerReady(context.Background(), server.Client(), server.URL, "codex", time.Now(), false)
				if ready {
					t.Fatal("old worker heartbeat must not count after server restart")
				}
			}
		})
	}
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	if running, _, err := localWorkerReady(context.Background(), server.Client(), server.URL, "codex", time.Time{}, false); running || err != nil {
		t.Fatalf("stopped server: running=%t err=%v", running, err)
	}
}

// The worker `jig start` launches talks to the server start checks, even
// when $JIG_SERVER names another one: the worker's real flag set (its
// --server default read from the environment) parses start's real args.
func TestStartedWorkerUsesTheStartupServerNotJIGServer(t *testing.T) {
	t.Setenv(serverEnv, "http://elsewhere.invalid:9999")
	var server string
	probe := func(flags *flag.FlagSet) {
		if err := flags.Parse(startWorkerArgs("codex", "http://127.0.0.1:8600", t.TempDir(), false)); err != nil {
			t.Fatal(err)
		}
		server = flags.Lookup("server").Value.String()
	}
	flagProbe.Store(&probe)
	defer flagProbe.Store(nil)
	workerCommand(context.Background(), nil, io.Discard, io.Discard)
	if server != "http://127.0.0.1:8600" {
		t.Fatalf("the started worker connects to %q, want the startup server", server)
	}
}
