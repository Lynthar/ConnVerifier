// Package runner decides whether checks may start and in what order. A stress
// check — one that can disturb other users of the network — starts only after the
// user has seen what it will do and agreed.
package runner

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/Lynthar/ConnVerifier/internal/probe/baseline"
	"github.com/Lynthar/ConnVerifier/internal/probe/gateway"
	"github.com/Lynthar/ConnVerifier/internal/probe/load"
	"github.com/Lynthar/ConnVerifier/internal/probe/nodeclient"
	"github.com/Lynthar/ConnVerifier/internal/result"
)

var (
	ErrDeclined = errors.New("declined at the confirmation prompt")
	ErrNeedsYes = errors.New("stdin is not a terminal and -yes was not given")
)

// checkIDs lists the check command's checks in the order Check runs them; the
// first nodeFree need no node.
var checkIDs = []string{gateway.ID, baseline.UDPID, baseline.TCPID, load.TCPID, load.QUICID}

const nodeFree = 1

// Config selects the checks Check runs.
type Config struct {
	only string // comma-separated check IDs; empty runs them all
}

func (cfg *Config) RegisterFlags(fs *flag.FlagSet) {
	fs.StringVar(&cfg.only, "only", "", "run only these checks, comma-separated: "+strings.Join(checkIDs, ", "))
}

// Validate reports the first ID in -only that names no check, or why no invite
// was found (node is the -node value) when every check selected needs one.
func (cfg Config) Validate(node string) error {
	for _, id := range cfg.ids() {
		if !slices.Contains(checkIDs, id) {
			return fmt.Errorf("-only: no check %q (the checks are %s)", id, strings.Join(checkIDs, ", "))
		}
	}
	if _, err := nodeclient.ResolveInvite(node); err != nil && !slices.ContainsFunc(checkIDs[:nodeFree], cfg.Selected) {
		return err
	}
	return nil
}

func (cfg Config) ids() []string {
	var ids []string
	for id := range strings.SplitSeq(cfg.only, ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// Selected reports whether check id runs.
func (cfg Config) Selected(id string) bool {
	ids := cfg.ids()
	return len(ids) == 0 || slices.Contains(ids, id)
}

// Loads counts the load checks that run: selected, with an invite to run them on.
func (cfg Config) Loads(node string) int {
	if _, err := nodeclient.ResolveInvite(node); err != nil {
		return 0
	}
	n := 0
	for _, id := range []string{load.TCPID, load.QUICID} {
		if cfg.Selected(id) {
			n++
		}
	}
	return n
}

// Confirm writes notice to out and asks before a stress check starts. yes approves
// without asking. Otherwise only an interactive "y" approves: with no terminal to
// answer from, nobody can have read the notice, so the answer is ErrNeedsYes.
func Confirm(in io.Reader, out io.Writer, interactive, yes bool, notice, prompt string) error {
	fmt.Fprintln(out, notice)
	if yes {
		return nil
	}
	if !interactive {
		return ErrNeedsYes
	}
	fmt.Fprint(out, prompt)
	line, _ := bufio.NewReader(in).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes", "是":
		return nil
	}
	return ErrDeclined
}

// Check runs the check command's selected checks: the gateway, the idle
// baseline, then the load over TCP, then over QUIC. None overlaps another — a
// path under load is not idle, and two loads share one bottleneck — and each load
// compares its round trips with the baseline's, quic-load its uploads also with
// tcp-load's. The two baselines are one measurement, side by side in one session:
// either runs both. Checks not selected, or needing a node when no invite was
// given, are reported as SKIP. It returns an error only when a configuration is
// invalid.
func Check(ctx context.Context, cfg Config, base baseline.Config, ld load.Config, version string) ([]result.Check, error) {
	var checks []result.Check
	if cfg.Selected(gateway.ID) {
		checks = gateway.Run(ctx, gateway.Config{})
	} else {
		for _, family := range []string{"ipv4", "ipv6"} {
			checks = append(checks, skipped(gateway.ID, gateway.MethodVersion, result.Path{Family: family}, "check.not_selected"))
		}
	}
	node, _, _ := base.Shared()
	inv, noInvite := nodeclient.ResolveInvite(node)
	addr := ""
	if len(inv.Addrs) > 0 {
		addr = inv.Addrs[0]
	}
	skip := func(id string, methodVersion int, proto string, selected bool) result.Check {
		path := result.Path{Node: addr, Family: nodeclient.Family(addr), Protocol: proto}
		if selected {
			return skipped(id, methodVersion, path, "check.no_invite")
		}
		return skipped(id, methodVersion, path, "check.not_selected")
	}
	switch either := cfg.Selected(baseline.UDPID) || cfg.Selected(baseline.TCPID); {
	case either && noInvite == nil:
		bs, err := baseline.Run(ctx, base, version)
		if err != nil {
			return nil, err
		}
		checks = append(checks, bs...)
	default:
		checks = append(checks, skip(baseline.UDPID, baseline.MethodVersion, "udp", either), skip(baseline.TCPID, baseline.MethodVersion, "tcp", either))
	}
	for _, l := range []struct {
		id, proto string
		run       func(context.Context, load.Config, string, []result.Check) (result.Check, error)
	}{{load.TCPID, "tcp", load.Run}, {load.QUICID, "quic", load.RunQUIC}} {
		if noInvite != nil || !cfg.Selected(l.id) {
			checks = append(checks, skip(l.id, load.MethodVersion, l.proto, cfg.Selected(l.id)))
			continue
		}
		c, err := l.run(ctx, ld, version, checks)
		if err != nil {
			return nil, err
		}
		checks = append(checks, c)
	}
	return checks, nil
}

func skipped(id string, methodVersion int, path result.Path, why string) result.Check {
	return result.Check{
		ID: id, MethodVersion: methodVersion, Status: result.Skip, Path: path,
		Params:   map[string]any{},
		Metrics:  []result.Metric{},
		Warnings: []result.Message{{Key: why}},
	}
}
