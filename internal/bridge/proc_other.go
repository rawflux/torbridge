//go:build !windows

package bridge

import (
	"os"
	"os/exec"
)

// Prepare: the Tor Expert Bundle ships its own libssl/libcrypto/libevent next to tor.
func Prepare(cmd *exec.Cmd, torDir string) {
	cmd.Env = append(os.Environ(), "LD_LIBRARY_PATH="+torDir)
}
