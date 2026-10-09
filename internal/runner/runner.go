// Package runner decides whether checks may start and in what order. A stress
// check — one that can disturb other users of the network — starts only after the
// user has seen what it will do and agreed.
package runner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Lynthar/ConnVerifier/internal/probe/baseline"
	"github.com/Lynthar/ConnVerifier/internal/probe/load"
	"github.com/Lynthar/ConnVerifier/internal/result"
)

var (
	ErrDeclined = errors.New("declined at the confirmation prompt")
	ErrNeedsYes = errors.New("stdin is not a terminal and -yes was not given")
)

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

// Check runs the check command: the idle baseline, then the load. They never
// overlap — a path under load is not idle — and the load compares its round trips
// with the baseline's. It returns an error only when a configuration is invalid.
func Check(ctx context.Context, base baseline.Config, ld load.Config, version string) ([]result.Check, error) {
	checks, err := baseline.Run(ctx, base, version)
	if err != nil {
		return nil, err
	}
	c, err := load.Run(ctx, ld, version, checks)
	if err != nil {
		return nil, err
	}
	return append(checks, c), nil
}
