//go:build windows

package agentcli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/JianFeeeee/HologramAgent/internal/ptywin"
)

func defaultShell() string { return "cmd.exe" }

// windowsPty 基于 internal/ptywin（ConPTY）的终端后端。
type windowsPty struct {
	c         *ptywin.ConPty
	cmd       *exec.Cmd
	closeOnce sync.Once
}

// newCommandPty 创建 ConPTY 并在其上运行命令（cmd.exe /c <command>）。
func newCommandPty(command string, rows, cols uint16) (ptyTerm, *exec.Cmd, error) {
	cmdLine := windowsCommandLine(command)
	c, err := ptywin.Start(cmdLine, ptywin.ConPtyDimensions(int(cols), int(rows)))
	if err != nil {
		return nil, nil, fmt.Errorf("conpty start: %v", err)
	}

	cmdObj := exec.Command("cmd.exe")
	cmdObj.Process = &os.Process{Pid: c.Pid()}

	return &windowsPty{c: c, cmd: cmdObj}, cmdObj, nil
}

func windowsCommandLine(command string) string {
	return "cmd.exe /c " + command
}

func (p *windowsPty) Read(buf []byte) (int, error) {
	return p.c.Read(buf)
}

func (p *windowsPty) WriteString(s string) (int, error) {
	return p.c.Write([]byte(s))
}

func (p *windowsPty) Resize(rows, cols uint16) error {
	return p.c.Resize(int(cols), int(rows))
}

func (p *windowsPty) Running() bool {
	return p.c != nil && p.c.Running()
}

func (p *windowsPty) Kill() error {
	if p.c != nil {
		return p.c.Kill()
	}
	return nil
}

func (p *windowsPty) Close() error {
	var errs []string
	p.closeOnce.Do(func() {
		if p.c != nil {
			if err := p.c.Close(); err != nil {
				errs = append(errs, err.Error())
			}
			p.c = nil
		}
	})
	if len(errs) > 0 {
		return fmt.Errorf("close: %s", strings.Join(errs, "; "))
	}
	return nil
}
