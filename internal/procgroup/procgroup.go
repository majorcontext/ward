// Package procgroup runs a command as the leader of its own process group and kills the whole group on cancel.
package procgroup

import (
	"context"
	"os/exec"
	"syscall"
	"time"
)

const waitDelay = 3 * time.Second

// Run starts cmd in a new process group and waits for it. When ctx ends, Run calls stop (if not nil) and then kills
// every process of the group, so a child that the command started cannot outlive it. It returns ctx.Err() when it killed.
func Run(ctx context.Context, cmd *exec.Cmd, stop func()) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = waitDelay
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	killed := make(chan struct{})
	go func() {
		defer close(killed)
		select {
		case <-ctx.Done():
			if stop != nil {
				stop()
			}
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		case <-done:
		}
	}()
	err := cmd.Wait()
	close(done)
	<-killed
	if ctx.Err() != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return ctx.Err()
	}
	return err
}
