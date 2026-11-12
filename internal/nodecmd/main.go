package nodecmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Main runs the command line args (args[0] is the program name) and returns
// the exit status. It is the whole of cmd/virticle-node.
//
// A signal (SIGINT, SIGTERM, SIGHUP, SIGPIPE) cancels the context, removes the
// files the verb registered with Cleanup (part files of a write, a received
// file, a download) and ends the process with status 1.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	syscall.Umask(0o077)
	sh := &shared{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGPIPE)
	done := make(chan struct{})
	go func() {
		select {
		case <-sigs:
			cancel()
			sh.runCleanups()
			select {
			case <-done:
			case <-time.After(500 * time.Millisecond):
			}
			os.Exit(1)
		case <-done:
		}
	}()
	var rest []string
	if len(args) > 1 {
		rest = args[1:]
	}
	code := run(ctx, sh, DefaultConfig(), rest, stdin, stdout, stderr)
	close(done)
	sh.runCleanups()
	return code
}

// Run runs one verb with an explicit Config and returns the exit status. argv
// is the verb and its arguments (no program name). It installs no signal
// handler and does not change the umask: it is for tests.
func Run(cfg *Config, argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	sh := &shared{}
	code := run(context.Background(), sh, cfg, argv, stdin, stdout, stderr)
	sh.runCleanups()
	return code
}

func run(ctx context.Context, sh *shared, cfg *Config, argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	say := func(format string, a ...any) { fmt.Fprintf(stderr, "virticle-node: "+format+"\n", a...) }
	if len(argv) < 1 {
		say("usage: virticle-node VERB [ARGS]")
		return 2
	}
	fn, ok := verbs[argv[0]]
	if !ok {
		say("unknown subcommand")
		return 2
	}
	if stdin == nil {
		stdin = emptyReader{}
	}
	c := &Ctx{Args: argv[1:], Stdin: stdin, Stdout: stdout, Stderr: stderr, Cfg: cfg, Context: ctx, sh: sh}
	err := fn(c)
	if err == nil {
		return 0
	}
	if e, ok := err.(*Error); ok {
		if e.Msg != "" {
			say("%s", e.Msg)
		}
		return e.Code
	}
	say("%v", err)
	return 1
}

type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, io.EOF }
