package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"runtime"
	"time"
)

const startUsage = `Usage: jig start [--no-open] [--allow-merge]

Start Jig for one person on this machine: the control plane on
http://127.0.0.1:8383 (unless one already answers there, with GitHub polling
off) and a worker on your signed-in Codex, then open the UI in your browser.
Leave it running; Ctrl+C stops the worker and any server it started.

Flags:
  --no-open       do not open the browser; print the address instead
  --allow-merge   allow never-held jobs to merge when their definition
                  opts in

Exit codes:
  0  stopped cleanly on Ctrl+C, or Jig was already running and ready
  1  the server or worker could not start, or did not become ready
  2  usage error
`

// Use the existing commands so startup preserves their cancellation and
// worker-drain behavior. Stop the worker before stopping an owned server.
func startCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("jig start", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, startUsage) }
	noOpen := flags.Bool("no-open", false, "do not open the browser")
	allowMerge := flags.Bool("allow-merge", false, "allow exact-head merge for opted-in definitions")
	if err := parseFlags(flags, args); err != nil {
		return flagExit(err)
	}
	if flags.NArg() != 0 {
		return usageError(stderr, "jig start", fmt.Sprintf("unexpected argument %q", flags.Arg(0)), startUsage)
	}
	url := "http://" + defaultServerAuthority
	client := &http.Client{Timeout: time.Second}
	var heartbeatAfter time.Time
	running, ready, err := localCodexReady(ctx, client, url, heartbeatAfter, *allowMerge)
	if err != nil {
		fmt.Fprintln(stderr, "jig start:", err)
		return exitInfraFailed
	}
	serverCtx, stopServer := context.WithCancel(context.WithoutCancel(ctx))
	defer stopServer()
	var serverDone chan int
	if !running {
		// A restarted server retains recent heartbeats from stopped workers.
		heartbeatAfter = time.Now()
		fmt.Fprintln(stdout, "Starting Jig…")
		serverDone = make(chan int, 1)
		go func() { serverDone <- serveCommand(serverCtx, []string{"--no-github-poll"}, stdout, stderr) }()
		defer func() { stopServer(); <-serverDone }()
	}
	workerCtx, stopWorker := context.WithCancel(context.WithoutCancel(ctx))
	defer stopWorker()
	var workerDone chan int
	defer func() {
		if workerDone != nil {
			stopWorker()
			<-workerDone
		}
	}()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for !ready {
		if running && workerDone == nil {
			fmt.Fprintln(stdout, "Starting your signed-in Codex worker…")
			workerDone = make(chan int, 1)
			workerArgs := []string{"--runtime", "codex"}
			if *allowMerge {
				workerArgs = append(workerArgs, "--allow-merge")
			}
			go func() { workerDone <- workerCommand(workerCtx, workerArgs, stdout, stderr) }()
		}
		select {
		case <-ctx.Done():
			return exitAccepted
		case code := <-serverDone:
			serverDone <- code
			return exitInfraFailed
		case code := <-workerDone:
			workerDone <- code
			return exitInfraFailed
		case <-deadline.C:
			fmt.Fprintln(stderr, "Jig could not become ready in 30 seconds. Check the startup messages above.")
			return exitInfraFailed
		case <-ticker.C:
			running, ready, err = localCodexReady(ctx, client, url, heartbeatAfter, *allowMerge)
			if err != nil {
				fmt.Fprintln(stderr, "jig start:", err)
				return exitInfraFailed
			}
		}
	}
	fmt.Fprintf(stdout, "Jig is ready: %s\nChoose a project, describe your task, and press Start task.\n", url)
	if !*noOpen {
		var command string
		switch runtime.GOOS {
		case "darwin":
			command = "open"
		case "linux":
			command = "xdg-open"
		}
		if command != "" {
			openCtx, cancelOpen := context.WithTimeout(ctx, 10*time.Second)
			err := exec.CommandContext(openCtx, command, url).Run()
			cancelOpen()
			if err != nil {
				fmt.Fprintln(stderr, "Open this address in your browser:", url)
			}
		}
	}
	if workerDone == nil && serverDone == nil {
		return exitAccepted
	}
	fmt.Fprintln(stdout, "Leave this window open while using Jig. Press Ctrl+C to stop.")
	select {
	case <-ctx.Done():
		return exitAccepted
	case code := <-workerDone:
		workerDone <- code
		return exitInfraFailed
	case code := <-serverDone:
		serverDone <- code
		return exitInfraFailed
	}
}

func localCodexReady(ctx context.Context, client *http.Client, url string, heartbeatAfter time.Time, requireMerge bool) (running, ready bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/api/workers", nil)
	if err != nil {
		return false, false, err
	}
	response, err := client.Do(req)
	if err != nil {
		return false, false, nil
	}
	defer response.Body.Close()
	var fleet struct {
		Workers []struct {
			AllowMerge bool      `json:"allow_merge"`
			Live       bool      `json:"live"`
			Heartbeat  time.Time `json:"last_heartbeat"`
			Runtimes   []struct {
				Name string `json:"name"`
			} `json:"runtimes"`
		} `json:"workers"`
	}
	if response.StatusCode != http.StatusOK {
		return true, false, fmt.Errorf("port 8383 did not return Jig's worker status (%s)", response.Status)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&fleet); err != nil {
		return true, false, fmt.Errorf("cannot read Jig worker status: %w", err)
	}
	for _, worker := range fleet.Workers {
		for _, capability := range worker.Runtimes {
			if worker.Live && capability.Name == "codex" && (heartbeatAfter.IsZero() || !worker.Heartbeat.Before(heartbeatAfter)) {
				if requireMerge && !worker.AllowMerge {
					continue
				}
				return true, true, nil
			}
		}
	}
	return true, false, nil
}
