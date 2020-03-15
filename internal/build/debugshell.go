package build

import (
	"log"
	"os"
	"os/exec"
)

func (b *Ctx) maybeStartDebugShell(stage string, env []string) {
	if b.Debug != stage {
		return
	}
	log.Printf("starting debug shell because -debug=%s", b.Debug)
	cmd := exec.Command("bash", "-i")
	if b.Hermetic {
		cmd.Env = env
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Printf("debug command failed: %v", err)
	}
}
