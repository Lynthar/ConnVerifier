package netenv

import (
	"fmt"
	"os"
)

func ephemeralPorts() (int, bool) {
	data, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range")
	if err != nil {
		return 0, false
	}
	return parsePortRange(string(data))
}

// parsePortRange reads "first last" as /proc prints it.
func parsePortRange(s string) (int, bool) {
	var first, last int
	if _, err := fmt.Sscan(s, &first, &last); err != nil || last < first {
		return 0, false
	}
	return last - first + 1, true
}
