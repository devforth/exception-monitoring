//go:build unix

package logging

import (
	"os"
	"syscall"
	"unsafe"
)

// terminalWidth is the number of columns of the terminal file is, 0 when
// it is not one or the size is unknown.
func terminalWidth(file *os.File) int {
	var size struct{ rows, columns, x, y uint16 }

	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&size)))

	if errno != 0 {
		return 0
	}

	return int(size.columns)
}
