package service

import (
	"os/exec"
	"strconv"
)

func wbPrepareProcess(cmd *exec.Cmd) {}
func wbSignalProcess(cmd *exec.Cmd, force bool) error {
	if cmd.Process == nil {
		return nil
	}
	args := []string{"/PID", strconv.Itoa(cmd.Process.Pid), "/T"}
	if force {
		args = append(args, "/F")
	}
	return exec.Command("taskkill", args...).Run()
}
