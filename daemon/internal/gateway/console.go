package gateway

import (
	"bufio"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"
)

// ConsoleInput tells whether f can serve as console input (DESIGN 5.13): a terminal, a pipe or FIFO, or a regular
// file. It returns a Chinese description of what f is, for daemon.log.
func ConsoleInput(f *os.File) (bool, string, error) {
	if term.IsTerminal(int(f.Fd())) {
		return true, "终端", nil
	}
	info, err := f.Stat()
	if err != nil {
		return false, "", fmt.Errorf("无法读取标准输入的状态：%w", err)
	}
	mode := info.Mode()
	switch {
	case mode&os.ModeNamedPipe != 0:
		return true, "管道或 FIFO", nil
	case mode.IsRegular():
		return true, "普通文件", nil
	case mode&os.ModeCharDevice != 0:
		return false, "字符设备（例如 /dev/null）", nil
	case mode&os.ModeSocket != 0:
		return false, "socket", nil
	default:
		return false, mode.Type().String(), nil
	}
}

// UnavailableNotice is printed for every input line while the console adapter is disabled (DESIGN 5.13).
const UnavailableNotice = "控制台不可用：gateway 没有启用 console 适配器（[platforms.console] enabled = false）"

// PromptUnavailable reads in line by line and writes UnavailableNotice for each line until EOF.
func PromptUnavailable(in io.Reader, out io.Writer) error {
	r := bufio.NewReader(in)
	for {
		line, err := r.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		if line != "" {
			if _, werr := fmt.Fprintln(out, UnavailableNotice); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			return nil
		}
	}
}
