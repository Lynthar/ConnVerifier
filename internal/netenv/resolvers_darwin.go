package netenv

import (
	"context"
	"net/netip"
	"os/exec"
	"strings"
	"time"
)

// resolvers asks scutil for the system's DNS configuration: /etc/resolv.conf says
// itself that most processes on macOS do not read it.
func resolvers() ([]netip.Addr, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/scutil", "--dns").Output()
	if err != nil {
		return nil, err
	}
	return mainResolvers(string(out)), nil
}

// mainResolvers reads the nameservers of "resolver #1" in the first section of
// `scutil --dns`, the resolver for every name no other one claims.
func mainResolvers(out string) []netip.Addr {
	var list []netip.Addr
	in := false
	for line := range strings.Lines(out) {
		line = strings.TrimSpace(line)
		switch {
		case line == "resolver #1":
			in = true
		case in && (line == "" || strings.HasPrefix(line, "resolver #")):
			return list
		case in && strings.HasPrefix(line, "nameserver["):
			_, v, _ := strings.Cut(line, ":")
			if a, err := netip.ParseAddr(strings.TrimSpace(v)); err == nil {
				list = appendNew(list, a.Unmap())
			}
		}
	}
	return list
}
