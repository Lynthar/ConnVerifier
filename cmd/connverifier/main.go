package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"syscall"

	"github.com/Lynthar/ConnVerifier/internal/node"
	"github.com/Lynthar/ConnVerifier/internal/probe/capacity"
)

const usage = `usage: connverifier <command> [flags]

commands:
  capacity   hold N long-lived TCP connections against a node and report drops
  serve      run the echo node that capacity connects to
  version    print the build version

Run "connverifier <command> -h" for the flags of a command.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd {
	case "capacity":
		var cfg capacity.Config
		parse(cmd, args, cfg.RegisterFlags)
		err = capacity.Run(ctx, cfg)
	case "serve":
		var cfg node.Config
		parse(cmd, args, cfg.RegisterFlags)
		err = node.Serve(ctx, cfg)
	case "version":
		parse(cmd, args, func(*flag.FlagSet) {})
		info, _ := debug.ReadBuildInfo()
		fmt.Printf("connverifier %s %s %s/%s\n", buildVersion(info), runtime.Version(), runtime.GOOS, runtime.GOARCH)
	case "help", "-h", "-help", "--help":
		fmt.Fprint(os.Stdout, usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
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

// buildVersion reports the module version stamped by the go command, falling back
// to the VCS revision for local builds that carry no version.
func buildVersion(info *debug.BuildInfo) string {
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
