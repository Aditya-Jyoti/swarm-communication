//go:build linux

package main

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// cbaud masks the speed bits in c_cflag (CBAUD | CBAUDEX). The syscall package
// does not export it; the value is fixed by the Linux ABI (asm-generic/termbits.h).
const cbaud = 0o10017

// baudRates maps the rates the flag accepts to their termios constants.
var baudRates = map[int]uint32{
	9600:   syscall.B9600,
	19200:  syscall.B19200,
	38400:  syscall.B38400,
	57600:  syscall.B57600,
	115200: syscall.B115200,
	230400: syscall.B230400,
	460800: syscall.B460800,
	921600: syscall.B921600,
}

// openSerial opens a serial device raw, 8N1, at baud.
//
// Standard library only: termios through ioctl. os.OpenFile registers a
// character device with the runtime poller, so a Read blocked on a quiet port
// returns when the file is closed -- which is how the relay shuts down.
//
// HUPCL is cleared so closing the port does not drop DTR. On the ground unit's
// CP2102 the DTR and RTS lines drive the ESP32's reset circuit, and a relay
// that reset the board every time it reconnected would make the board's Wi-Fi
// rejoin, and the drone link restart, on every hiccup.
func openSerial(path string, baud int) (*os.File, error) {
	speed, ok := baudRates[baud]
	if !ok {
		return nil, fmt.Errorf("baud %d not supported", baud)
	}
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, err
	}
	var t syscall.Termios
	if err := ioctlTermios(f, syscall.TCGETS, &t); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%s: read termios: %w", path, err)
	}
	// cfmakeraw, then 8N1 at the requested speed.
	t.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	t.Oflag &^= syscall.OPOST
	t.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	t.Cflag &^= syscall.CSIZE | syscall.PARENB | syscall.CSTOPB | syscall.HUPCL | cbaud
	// The speed lives in c_cflag only. TCGETS/TCSETS use the kernel's legacy
	// struct termios, which has no c_ispeed/c_ospeed: the Ispeed and Ospeed
	// fields of syscall.Termios are never read by this ioctl.
	t.Cflag |= syscall.CS8 | syscall.CREAD | syscall.CLOCAL | speed
	t.Cc[syscall.VMIN] = 1
	t.Cc[syscall.VTIME] = 0
	if err := ioctlTermios(f, syscall.TCSETS, &t); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%s: write termios: %w", path, err)
	}
	return f, nil
}

func ioctlTermios(f *os.File, req uintptr, t *syscall.Termios) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var errno syscall.Errno
	if err := rc.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(unsafe.Pointer(t)))
	}); err != nil {
		return err
	}
	if errno != 0 {
		return errno
	}
	return nil
}
