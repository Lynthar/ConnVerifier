// Package runner decides whether checks may start. A stress check — one that can
// disturb other users of the network — starts only after the user has seen what
// it will do and agreed.
package runner

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
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
