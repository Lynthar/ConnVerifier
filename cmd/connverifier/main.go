package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/i18n"
	"github.com/Lynthar/ConnVerifier/internal/node"
	"github.com/Lynthar/ConnVerifier/internal/probe/baseline"
	"github.com/Lynthar/ConnVerifier/internal/probe/capacity"
	"github.com/Lynthar/ConnVerifier/internal/probe/load"
	"github.com/Lynthar/ConnVerifier/internal/report"
	"github.com/Lynthar/ConnVerifier/internal/result"
	"github.com/Lynthar/ConnVerifier/internal/runner"
	"golang.org/x/term"
)

const usage = `usage: connverifier <command> [flags]

commands:
  check      measure round trip, its variation and UDP loss to a node, idle and
             with the path loaded over TCP and over QUIC, and the goodput of
             each load
  capacity   hold N long-lived TCP connections against a node and report drops
  serve      run a node that clients measure against
  invite     create, list or revoke the invites a node accepts
  version    print the build version

Run "connverifier <command> -h" for the flags of a command.

Exit status: 0 when the run completed, 1 when a check obtained no valid
measurement or the tool failed, 2 when nothing ran: invalid flags, or a
stress check that was not confirmed.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "check":
		var cfg baseline.Config
		var ld load.Config
		var out output
		var yes bool
		parse(cmd, args, func(fs *flag.FlagSet) {
			cfg.RegisterFlags(fs)
			ld.RegisterFlags(fs)
			out.register(fs)
			fs.BoolVar(&yes, "yes", false, "start a run above the default traffic without the confirmation prompt")
		})
		invalidIf(cfg.Validate())
		ld.Use(cfg.Shared())
		invalidIf(ld.Validate())
		invalidIf(out.resolve())
		if ld.Large() {
			confirm(out.cat, ld.Notice(), yes)
		} else {
			fmt.Fprintln(os.Stderr, report.Message(out.cat, ld.Notice()))
		}
		started := time.Now()
		checks, err := runner.Check(ctx, cfg, ld, buildVersion())
		if err != nil {
			log.Fatal(err)
		}
		run := newRun(started, time.Now(), checks...)
		if err := out.write(os.Stdout, run); err != nil {
			log.Fatal(err)
		}
		stop()
		os.Exit(run.ExitCode())
	case "capacity":
		var cfg capacity.Config
		var out output
		var yes bool
		parse(cmd, args, func(fs *flag.FlagSet) {
			cfg.RegisterFlags(fs)
			out.register(fs)
			fs.BoolVar(&yes, "yes", false, "start without the confirmation prompt (required when stdin is not a terminal)")
		})
		invalidIf(cfg.Validate())
		invalidIf(out.resolve())
		confirm(out.cat, cfg.Notice(), yes)
		started := time.Now()
		check, err := capacity.Run(ctx, cfg, buildVersion())
		if err != nil {
			log.Fatal(err)
		}
		run := newRun(started, time.Now(), check)
		if err := out.write(os.Stdout, run); err != nil {
			log.Fatal(err)
		}
		stop()
		os.Exit(run.ExitCode())
	case "serve":
		var cfg node.Config
		parse(cmd, args, cfg.RegisterFlags)
		invalidIf(cfg.Validate())
		if err := node.Serve(ctx, cfg, buildVersion()); err != nil {
			log.Fatal(err)
		}
	case "invite":
		runInvite(args)
	case "version":
		parse(cmd, args, func(*flag.FlagSet) {})
		fmt.Printf("connverifier %s %s %s/%s quic-go %s\n", buildVersion(), runtime.Version(), runtime.GOOS, runtime.GOARCH, load.QUICGoVersion())
	case "help", "-h", "-help", "--help":
		fmt.Fprint(os.Stdout, usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
}

// parse parses one command's flags; flag errors and stray arguments exit with status 2.
func parse(name string, args []string, register func(*flag.FlagSet)) {
	fs := flag.NewFlagSet("connverifier "+name, flag.ExitOnError)
	register(fs)
	fs.Parse(args)
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		os.Exit(2)
	}
}

// confirm runs the stress-check prompt on stderr, keeping stdout for the result,
// and exits 2 unless the user agrees.
func confirm(cat *i18n.Catalog, notice result.Message, yes bool) {
	interactive := term.IsTerminal(int(os.Stdin.Fd()))
	err := runner.Confirm(os.Stdin, os.Stderr, interactive, yes, report.Message(cat, notice), cat.Text("confirm.prompt", nil))
	switch {
	case err == nil:
		return
	case errors.Is(err, runner.ErrNeedsYes):
		fmt.Fprintln(os.Stderr, cat.Text("confirm.needs_yes", nil))
	default:
		fmt.Fprintln(os.Stderr, cat.Text("confirm.declined", nil))
	}
	os.Exit(2)
}

func invalidIf(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid configuration: %v\n", err)
		os.Exit(2)
	}
}

// output holds the flags that choose how a result is written.
type output struct {
	format string
	lang   string
	cat    *i18n.Catalog
}

func (o *output) register(fs *flag.FlagSet) {
	fs.StringVar(&o.format, "format", "text", "result format: text or json")
	fs.StringVar(&o.lang, "lang", "", "report language: "+strings.Join(i18n.Supported, " or ")+" (default: from LC_ALL, LC_MESSAGES or LANG, else "+i18n.Default+")")
}

// resolve rejects unknown values rather than ignoring them and loads the catalog.
func (o *output) resolve() error {
	if o.format != "text" && o.format != "json" {
		return fmt.Errorf("format must be text or json, got %q", o.format)
	}
	lang := i18n.Detect(os.Getenv)
	if o.lang != "" {
		l, ok := i18n.Normalize(o.lang)
		if !ok {
			return fmt.Errorf("lang must be %s, got %q", strings.Join(i18n.Supported, " or "), o.lang)
		}
		lang = l
	}
	cat, err := i18n.Load(lang)
	if err != nil {
		return err
	}
	o.cat = cat
	return nil
}

func (o *output) write(w io.Writer, run result.Run) error {
	if o.format == "json" {
		return report.JSON(w, run)
	}
	return report.Text(w, run, o.cat)
}

// newRun wraps checks in the run envelope. The run ID is random per run, so nothing
// ties two runs together.
func newRun(started, ended time.Time, checks ...result.Check) result.Run {
	return result.Run{
		Schema: result.Schema,
		Tool: result.Tool{
			Name:    "connverifier",
			Version: buildVersion(),
			Go:      runtime.Version(),
			OS:      runtime.GOOS,
			Arch:    runtime.GOARCH,
		},
		RunID:     rand.Text(),
		Started:   started.UTC(),
		Ended:     ended.UTC(),
		ElapsedMs: ended.Sub(started).Milliseconds(),
		Checks:    checks,
	}
}

func buildVersion() string {
	info, _ := debug.ReadBuildInfo()
	return versionOf(info)
}

// versionOf reports the module version stamped by the go command, falling back
// to the VCS revision for local builds that carry no version.
func versionOf(info *debug.BuildInfo) string {
	if info == nil {
		return "unknown"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	var rev, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if rev == "" {
		return "devel"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if modified == "true" {
		rev += "+dirty"
	}
	return "devel-" + rev
}
