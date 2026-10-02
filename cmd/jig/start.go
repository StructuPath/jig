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

// Use the existing commands so startup preserves their cancellation and
// worker-drain behavior. Stop the worker before stopping an owned server.
func startCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("jig start", flag.ContinueOnError)
	flags.SetOutput(stderr)
	noOpen := flags.Bool("no-open", false, "do not open the browser")
	if err := flags.Parse(args); err != nil {
		return exitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "Usage: jig start [--no-open]")
		return exitUsage
	}
	url := "http://" + defaultServerAuthority
	client := &http.Client{Timeout: time.Second}
	var heartbeatAfter time.Time
	running, ready, err := localCodexReady(ctx, client, url, heartbeatAfter)
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
			go func() { workerDone <- workerCommand(workerCtx, []string{"--runtime", "codex"}, stdout, stderr) }()
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
			running, ready, err = localCodexReady(ctx, client, url, heartbeatAfter)
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

func localCodexReady(ctx context.Context, client *http.Client, url string, heartbeatAfter time.Time) (running, ready bool, err error) {
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
			Live      bool      `json:"live"`
			Heartbeat time.Time `json:"last_heartbeat"`
			Runtimes  []struct {
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
				return true, true, nil
			}
		}
	}
	return true, false, nil
}
