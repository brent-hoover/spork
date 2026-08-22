package agent

import (
	"errors"
	"os/exec"
	"sort"
)

func asExitError(err error, target **exec.ExitError) bool { return errors.As(err, target) }

func sorted(s []string) []string {
	out := append([]string{}, s...)
	sort.Strings(out)
	return out
}
