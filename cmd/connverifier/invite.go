package main

import (
	"flag"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/i18n"
	"github.com/Lynthar/ConnVerifier/internal/node"
)

const inviteUsage = `usage: connverifier invite <create|list|revoke> [flags]

  create -label NAME -addr HOST:PORT [-addr …]   print a new invite string
  list                                             list this node's invites
  revoke -label NAME                               stop accepting an invite
`

type addrList []string

func (a *addrList) String() string     { return strings.Join(*a, ",") }
func (a *addrList) Set(s string) error { *a = append(*a, s); return nil }

// runInvite manages invites in a node's state directory; it runs on the node host.
func runInvite(args []string) {
	if len(args) < 1 {
		fmt.Fprint(os.Stderr, inviteUsage)
		os.Exit(2)
	}
	sub, args := args[0], args[1:]
	var (
		stateDir, label, lang string
		addrs                 addrList
		lim                   = node.DefaultInviteLimits
		maxDuration, maxIdle  time.Duration
		udpPort               int
		loadMB, loadDayMB     = lim.MaxLoadBytes / 1e6, lim.MaxLoadBytesPerDay / 1e6
	)
	register := func(fs *flag.FlagSet) {
		fs.StringVar(&stateDir, "state-dir", node.DefaultStateDir(), "node state directory")
		fs.StringVar(&lang, "lang", "", "message language: "+strings.Join(i18n.Supported, " or "))
		if sub == "create" || sub == "revoke" {
			fs.StringVar(&label, "label", "", "name of the invite, shown to its holder and in results")
		}
		if sub == "create" {
			fs.Var(&addrs, "addr", "HOST:PORT clients use to reach this node (repeatable)")
			fs.IntVar(&lim.MaxSessions, "max-sessions", lim.MaxSessions, "concurrent sessions for this invite")
			fs.IntVar(&lim.MaxConnections, "max-connections", lim.MaxConnections, "live connections per session")
			fs.IntVar(&lim.MaxDialRate, "max-dial-rate", lim.MaxDialRate, "new connections per second per session")
			fs.DurationVar(&maxDuration, "max-duration", time.Duration(lim.MaxDurationS)*time.Second, "longest session")
			fs.DurationVar(&maxIdle, "max-idle", time.Duration(lim.MaxIdleTimeoutS)*time.Second, "longest idle period a session may ask for")
			fs.IntVar(&lim.MaxStampRate, "max-stamp-rate", lim.MaxStampRate, "STAMP packets per second per session (0: no UDP checks)")
			fs.IntVar(&udpPort, "udp-port", 0, "UDP port of the node's STAMP reflector (default: the first -addr's port)")
			fs.Int64Var(&loadMB, "max-load-mb", loadMB, "load check traffic per session, both directions, in MB (0: no load check)")
			fs.Int64Var(&loadDayMB, "max-load-mb-per-day", loadDayMB, "load check traffic per UTC day for this invite, in MB; counted in memory, reset when the node restarts")
			fs.IntVar(&lim.MaxLoadConnections, "max-load-connections", lim.MaxLoadConnections, "load and probe connections per session")
		}
	}
	switch sub {
	case "create", "list", "revoke":
	default:
		fmt.Fprintf(os.Stderr, "unknown invite command %q\n\n%s", sub, inviteUsage)
		os.Exit(2)
	}
	parse("invite "+sub, args, register)
	cat := loadCatalog(lang)

	switch sub {
	case "create":
		if label == "" {
			invalidIf(fmt.Errorf("-label is required"))
		}
		if len(addrs) == 0 {
			fmt.Fprintln(os.Stderr, cat.Text("invite.need_addr", map[string]any{"addrs": publicAddrs()}))
			os.Exit(2)
		}
		lim.MaxDurationS = int(maxDuration / time.Second)
		lim.MaxIdleTimeoutS = int(maxIdle / time.Second)
		lim.MaxLoadBytes, lim.MaxLoadBytesPerDay = loadMB*1e6, loadDayMB*1e6
		if udpPort == 0 && lim.MaxStampRate > 0 {
			_, port, err := net.SplitHostPort(addrs[0])
			invalidIf(err)
			udpPort, _ = strconv.Atoi(port)
		}
		if lim.MaxStampRate == 0 {
			udpPort = 0
		}
		inv, err := node.CreateInvite(stateDir, label, addrs, udpPort, lim)
		invalidIf(err)
		fmt.Println(inv.Encode())
		fmt.Fprintln(os.Stderr, cat.Text("invite.created_note", map[string]any{"label": label}))
	case "list":
		invites, err := node.ListInvites(stateDir)
		invalidIf(err)
		if len(invites) == 0 {
			fmt.Println(cat.Text("invite.list_empty", nil))
		}
		for _, r := range invites {
			fmt.Println(cat.Text("invite.list_row", map[string]any{
				"label": r.Label, "created": r.Created.Format(time.DateOnly),
				"sessions": r.Limits.MaxSessions, "connections": r.Limits.MaxConnections,
				"rate": r.Limits.MaxDialRate, "duration": shortDuration(r.Limits.MaxDurationS),
				"idle":    shortDuration(r.Limits.MaxIdleTimeoutS),
				"load_mb": r.Limits.MaxLoadBytes / 1e6, "load_day_mb": r.Limits.MaxLoadBytesPerDay / 1e6,
			}))
		}
	case "revoke":
		if label == "" {
			invalidIf(fmt.Errorf("-label is required"))
		}
		invalidIf(node.RevokeInvite(stateDir, label))
	}
}

// publicAddrs lists this host's globally routable, non-private addresses; the
// operator confirms which of them clients can actually reach.
func publicAddrs() string {
	ifaddrs, err := net.InterfaceAddrs()
	if err != nil {
		return "-"
	}
	var out []string
	for _, a := range ifaddrs {
		prefix, err := netip.ParsePrefix(a.String())
		if err != nil {
			continue
		}
		ip := prefix.Addr()
		if ip.IsGlobalUnicast() && !ip.IsPrivate() {
			out = append(out, ip.String())
		}
	}
	if len(out) == 0 {
		return "-"
	}
	return strings.Join(out, ", ")
}

// shortDuration prints whole hours or minutes without the trailing zero units.
func shortDuration(seconds int) string {
	d := time.Duration(seconds) * time.Second
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return d.String()
}

func loadCatalog(lang string) *i18n.Catalog {
	tag := i18n.Detect(os.Getenv)
	if lang != "" {
		l, ok := i18n.Normalize(lang)
		if !ok {
			invalidIf(fmt.Errorf("lang must be %s, got %q", strings.Join(i18n.Supported, " or "), lang))
		}
		tag = l
	}
	cat, err := i18n.Load(tag)
	invalidIf(err)
	return cat
}
