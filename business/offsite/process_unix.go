//go:build darwin || linux

package offsite

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Restic starts the exporter itself, so cancellation must own their entire process group.
func configureProcess(command *exec.Cmd) error {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.WaitDelay = 5 * time.Second
	command.Cancel = func() error { return terminateProcess(command) }
	return nil
}
func terminateProcess(command *exec.Cmd) error {
	if command.Process == nil {
		return nil
	}
	err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
