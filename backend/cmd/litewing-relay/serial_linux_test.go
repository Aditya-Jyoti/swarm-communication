//go:build linux

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// openPTY returns a pseudo-terminal pair: what is written to master arrives on
// the slave path, which openSerial opens like any USB serial device.
func openPTY(t *testing.T) (*os.File, string) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pty support: %v", err)
	}
	t.Cleanup(func() { _ = master.Close() })
	var unlock int32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); e != 0 {
		t.Fatalf("unlockpt: %v", e)
	}
	var n uint32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); e != 0 {
		t.Fatalf("ptsname: %v", e)
	}
	return master, fmt.Sprintf("/dev/pts/%d", n)
}

func TestOpenSerialIsRawAtTheBaud(t *testing.T) {
	master, path := openPTY(t)
	f, err := openSerial(path, 115200)
	if err != nil {
		t.Fatalf("openSerial: %v", err)
	}
	defer f.Close()

	var tio syscall.Termios
	if err := ioctlTermios(f, syscall.TCGETS, &tio); err != nil {
		t.Fatal(err)
	}
	if tio.Lflag&(syscall.ICANON|syscall.ECHO|syscall.ISIG) != 0 {
		t.Fatalf("not raw: lflag %#x", tio.Lflag)
	}
	if tio.Iflag&(syscall.ICRNL|syscall.IXON) != 0 || tio.Oflag&syscall.OPOST != 0 {
		t.Fatalf("not raw: iflag %#x oflag %#x", tio.Iflag, tio.Oflag)
	}
	if tio.Cflag&syscall.CSIZE != syscall.CS8 || tio.Cflag&syscall.PARENB != 0 || tio.Cflag&syscall.HUPCL != 0 {
		t.Fatalf("not 8N1: cflag %#x", tio.Cflag)
	}
	if tio.Cflag&cbaud != syscall.B115200 {
		t.Fatalf("baud: cflag %#x", tio.Cflag)
	}

	// Raw means a "\r\n" arrives as written: canonical mode would turn the
	// CR into a second newline.
	if _, err := io.WriteString(master, "TLM {}\r\n"); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(f)
	got, err := r.ReadString('\n')
	if err != nil || got != "TLM {}\r\n" {
		t.Fatalf("read %q, %v", got, err)
	}
}

func TestOpenSerialCloseUnblocksRead(t *testing.T) {
	_, path := openPTY(t)
	f, err := openSerial(path, 9600)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.Read(make([]byte, 8))
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	_ = f.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("read returned no error after close")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock Read: the relay could not shut down")
	}
}

func TestOpenSerialErrors(t *testing.T) {
	if _, err := openSerial("/nonexistent/tty", 115200); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing device: %v", err)
	}
	// A regular file is not a terminal: TCGETS fails and the file is closed.
	tmp := t.TempDir() + "/notatty"
	if err := os.WriteFile(tmp, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openSerial(tmp, 115200); err == nil || !strings.Contains(err.Error(), "termios") {
		t.Fatalf("regular file: %v", err)
	}
	_, path := openPTY(t)
	if _, err := openSerial(path, 12345); err == nil {
		t.Fatal("unsupported baud accepted")
	}
}

func TestIoctlOnClosedFile(t *testing.T) {
	_, path := openPTY(t)
	f, err := openSerial(path, 115200)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	var tio syscall.Termios
	if err := ioctlTermios(f, syscall.TCGETS, &tio); err == nil {
		t.Fatal("ioctl on a closed file succeeded")
	}
}

func TestRealMain(t *testing.T) {
	if code := realMain(context.Background(), []string{"-baud", "7"}, io.Discard); code != 2 {
		t.Fatalf("bad flags: exit %d, want 2", code)
	}
	if code := realMain(context.Background(), []string{"-to", "relay-test.invalid:1"}, io.Discard); code != 1 {
		t.Fatalf("unresolvable target: exit %d, want 1", code)
	}
	// A real run against a pty: one line in, one datagram out, then cancel.
	master, path := openPTY(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	go func() {
		time.Sleep(50 * time.Millisecond)
		_, _ = io.WriteString(master, line+"\n")
	}()
	if code := realMain(ctx, []string{"-port", path, "-to", "127.0.0.1:9"}, io.Discard); code != 0 {
		t.Fatalf("run: exit %d", code)
	}
}
