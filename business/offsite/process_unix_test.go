//go:build darwin || linux

package offsite

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/matryer/is"
)

// TestProcessCancellationOwnsDescendants proves cancellation stops a real producer child rather than only its parent.
func TestProcessCancellationOwnsDescendants(t *testing.T) {
	assert := is.New(t)
	file := filepath.Join(t.TempDir(), "producer-pid")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/sh", "-c", `sleep 300 & printf '%s' "$!" > "$1"; wait`, "producer", file)
	assert.NoErr(configureProcess(command))
	assert.NoErr(command.Start())
	defer terminateProcess(command)
	deadline := time.Now().Add(5 * time.Second)
	child := 0
	for child == 0 {
		data, _ := os.ReadFile(file)
		child, _ = strconv.Atoi(string(data))
		if time.Now().After(deadline) {
			t.Fatal("producer child did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	assert.True(command.Wait() != nil)
	deadline = time.Now().Add(5 * time.Second)
	for {
		err := syscall.Kill(child, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		// A killed Unix child can briefly remain as a zombie while its new parent reaps it.
		status, _ := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(child)).Output()
		if strings.HasPrefix(strings.TrimSpace(string(status)), "Z") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("producer child survived cancellation")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
