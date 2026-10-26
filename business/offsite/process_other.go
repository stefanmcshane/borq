//go:build !darwin && !linux

package offsite

import (
	"errors"
	"os/exec"
)

func configureProcess(command *exec.Cmd) error {
	return errors.New("offsite repository processes require Linux or macOS process-group ownership")
}
func terminateProcess(command *exec.Cmd) error { return nil }
