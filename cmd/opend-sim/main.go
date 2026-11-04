// Command opend-sim serves a synthetic OpenD on a loopback port, for Desk and CLI
// witnesses of the OpenD path. It speaks the real framing and protobuf against the
// synthetic book in pkg/futu/futusim. It is not part of the release binaries, reads
// nothing from the machine it runs on, and never implements the trade unlock.
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

	"github.com/osauer/canary/v2/pkg/futu/futusim"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run parses the flags and serves until a signal; it returns the exit status.
func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("opend-sim", flag.ContinueOnError)
	flags.SetOutput(stderr)
	listen := flags.String("listen", "127.0.0.1:0", "address to listen on; port 0 picks a free one")
	bookPath := flags.String("book", "", "book JSON; empty is the embedded synthetic book")
	clock := flags.String("clock", "fixed", "fixed holds the book's clock; now follows the wall clock")
	tier := flags.Int("tier", 0, "subscription and history quota; 0 keeps the book's")
	record := flags.String("record", "", "write the session's frames to this fixture when stopped")
	lockReal := flags.Bool("lock-real", true, "keep the book's real accounts locked for trading")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *clock != "fixed" && *clock != "now" {
		fmt.Fprintf(stderr, "opend-sim: -clock must be fixed or now, got %q\n", *clock)
		return 2
	}

	book := futusim.Default()
	if *bookPath != "" {
		loaded, err := futusim.Load(*bookPath)
		if err != nil {
			fmt.Fprintln(stderr, "opend-sim:", err)
			return 1
		}
		book = loaded
	}
	if *tier > 0 {
		book.SetSubscriptionQuota(*tier)
		book.SetHistoryQuota(*tier)
	}
	book.SetRealLocked(*lockReal)
	if *clock == "now" {
		book.FollowWallClock(time.Now())
	}

	opts := futusim.Options{Listen: *listen, Book: book}
	var recorder *futusim.Recorder
	if *record != "" {
		recorder = &futusim.Recorder{}
		opts.Record = recorder
	}
	sim, err := futusim.Listen(opts)
	if err != nil {
		fmt.Fprintln(stderr, "opend-sim:", err)
		return 1
	}
	defer sim.Close()
	fmt.Fprintln(stdout, sim.Addr())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	serve(ctx, sim, *clock == "now")

	if recorder != nil {
		if err := futusim.WriteScript(*record, recorder.Steps()); err != nil {
			fmt.Fprintln(stderr, "opend-sim:", err)
			return 1
		}
	}
	return 0
}

// serve waits for the signal; on a running clock it pushes quotes once a second, as
// OpenD pushes a quote when a price moves.
func serve(ctx context.Context, sim *futusim.Server, pushing bool) {
	if !pushing {
		<-ctx.Done()
		return
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			sim.PushQuotes()
			sim.Book().Settle()
		}
	}
}
