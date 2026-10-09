//go:build !unix && !windows

package netenv

import "time"

func processCPU() (time.Duration, bool) { return 0, false }
