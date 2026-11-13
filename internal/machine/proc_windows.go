package machine

import (
	"os/exec"
	"syscall"
)

// A machine is a Linux container; these only keep the CLI building where it
// is never a machine's entry point.

func ownGroup(*exec.Cmd) {}

func signalGroup(cmd *exec.Cmd, _ syscall.Signal) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
