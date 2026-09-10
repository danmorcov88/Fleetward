// Command demo runs the Fleetward demo against a real development stack.
//
//	go run ./tools/demo            # bring the stack up, tell the story, tear it down
//	go run ./tools/demo -keep      # leave the stack running afterwards
//	go run ./tools/demo -transcript demo.txt
//
// It is the same program the end-to-end test runs (ADR-0037). Everything it narrates it also
// asserts, so a demo that has quietly stopped working fails rather than lying, and a non-zero exit
// means an act did not hold.
//
// To record it: `asciinema rec fleetward-demo.cast -c 'go run ./tools/demo'`, or `-transcript` for
// a plain text file when asciinema is not installed. No dependency is worth adding for this.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/danmorcov88/fleetward/tools/demo/acts"
)

func main() { os.Exit(run()) }

// run is separate from main so that the deferred teardown and the signal handler's own stop
// actually run: os.Exit skips defers, and skipping them here would leave a stack behind.
func run() int {
	cfg, err := acts.DefaultConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "demo: %v\n", err)
		return 2
	}
	cfg.Theatre = true

	var transcript, cast string
	var idle time.Duration
	flag.BoolVar(&cfg.Keep, "keep", false,
		"leave the stack running afterwards, so the estate view can still be clicked around")
	flag.BoolVar(&cfg.Compose, "compose", true,
		"bring the stack up and take it down; off to run against a stack that is already up")
	flag.BoolVar(&cfg.Build, "build", false,
		"rebuild the control plane and web images before starting")
	flag.DurationVar(&cfg.Pause, "pause", 2*time.Second, "how long to pause between acts")
	flag.StringVar(&transcript, "transcript", "", "also write everything to this `file`")
	flag.StringVar(&cast, "cast", "",
		"also record an asciicast v2 `file`, which `agg` renders to a GIF")
	flag.DurationVar(&idle, "cast-idle-limit", 4*time.Second,
		"longest gap a player replays from the recording; the real timings stay in the file")
	flag.StringVar(&cfg.ServerURL, "server", cfg.ServerURL, "control plane base URL")
	flag.Parse()

	// Both are tees rather than redirections: the demo is something a person watches, and a run
	// that recorded itself and showed nothing would be a strange thing to sit through.
	sinks := []io.Writer{os.Stdout}
	if transcript != "" {
		file, err := os.Create(transcript) //nolint:gosec // G304: operator-supplied output path
		if err != nil {
			fmt.Fprintf(os.Stderr, "demo: %v\n", err)
			return 2
		}
		defer func() { _ = file.Close() }()
		sinks = append(sinks, file)
	}
	if cast != "" {
		recorder, closeCast, err := acts.NewCastRecorder(cast, idle)
		if err != nil {
			fmt.Fprintf(os.Stderr, "demo: %v\n", err)
			return 2
		}
		defer func() {
			if err := closeCast(); err != nil {
				fmt.Fprintf(os.Stderr, "demo: the recording is incomplete: %v\n", err)
			}
		}()
		sinks = append(sinks, recorder)
	}
	cfg.Out = io.MultiWriter(sinks...)

	// Ctrl-C has to reach the teardown rather than kill the process, or an interrupted demo leaves
	// a stack and a sandbox container behind.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := acts.Run(ctx, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "\ndemo: %v\n", err)
		return 1
	}
	return 0
}
