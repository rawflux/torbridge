package bridge

import (
	"os/exec"
	"syscall"
)

// Prepare: test tor processes must not open console windows.
func Prepare(cmd *exec.Cmd, torDir string) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}
