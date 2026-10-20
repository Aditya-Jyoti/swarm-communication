// Command litewing-relay carries the ground unit's LiteWing telemetry from its
// USB serial port to the Control Center's MAVLink bridge.
//
// The handheld (esp/esp.ino in LiteWing mode) prints one "TLM {json}" line per
// sample. This reads those lines, validates each with pkg/litewing, and sends
// the bare JSON object as one UDP datagram to the bridge's live-feed port
// (MAV_LIVE_BIND, published on 127.0.0.1:14560). Everything else the board
// prints is its own log output and is ignored.
//
// It runs on the host, not in a container, because the USB device does not
// stay put: it re-enumerates on a brown-out or a replug, and its permissions
// reset each time. A host process just reopens it, every -retry, for as long as
// it takes.
//
//	go run ./cmd/litewing-relay -port /dev/ttyUSB0
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"swarm-net/pkg/litewing"
)

const (
	exitOK      = 0
	exitRuntime = 1
	exitConfig  = 2
)

type config struct {
	Port   string
	Baud   int
	To     string
	Retry  time.Duration
	Report time.Duration
}

func parseFlags(args []string, stderr io.Writer) (config, error) {
	fs := flag.NewFlagSet("litewing-relay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var c config
	fs.StringVar(&c.Port, "port", "/dev/ttyUSB0", "the ground unit's serial device")
	fs.IntVar(&c.Baud, "baud", 115200, "serial baud rate; the sketch uses 115200")
	fs.StringVar(&c.To, "to", "127.0.0.1:14560", "the bridge's live-feed UDP address (MAV_LIVE_BIND, as published)")
	fs.DurationVar(&c.Retry, "retry", time.Second, "wait between attempts to (re)open the port")
	fs.DurationVar(&c.Report, "report", 10*time.Second, "period of the stats line; 0 turns it off")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if fs.NArg() > 0 {
		return config{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if c.Port == "" {
		return config{}, errors.New("-port is empty")
	}
	if _, ok := baudRates[c.Baud]; !ok {
		return config{}, fmt.Errorf("-baud %d is not a standard rate", c.Baud)
	}
	host, portStr, err := net.SplitHostPort(c.To)
	if err != nil {
		return config{}, fmt.Errorf("-to: %v", err)
	}
	if p, err := strconv.Atoi(portStr); err != nil || p < 1 || p > 65535 || host == "" {
		return config{}, fmt.Errorf("-to %q: want host:port with a port in 1..65535", c.To)
	}
	if c.Retry <= 0 {
		return config{}, errors.New("-retry must be positive")
	}
	if c.Report < 0 {
		return config{}, errors.New("-report must not be negative")
	}
	return c, nil
}

// stats are atomics because the reporter reads them while run writes them.
type stats struct {
	lines, forwarded, rejected, sendErrs, opens, openErrors atomic.Uint64
}

type statsSnapshot struct {
	Lines, Forwarded, Rejected, SendErrors, Opens, OpenErrors uint64
}

func (s *stats) snapshot() statsSnapshot {
	return statsSnapshot{
		Lines: s.lines.Load(), Forwarded: s.forwarded.Load(), Rejected: s.rejected.Load(),
		SendErrors: s.sendErrs.Load(), Opens: s.opens.Load(), OpenErrors: s.openErrors.Load(),
	}
}

// run opens the port, pumps it until it fails, and reopens it, until ctx ends.
// It returns nil on cancellation: losing the port is the normal case here, not
// an error.
func run(ctx context.Context, cfg config, open func() (io.ReadCloser, error), send func([]byte) error, log *slog.Logger, st *stats) error {
	if cfg.Report > 0 {
		go report(ctx, cfg.Report, log, st)
	}
	lastErr := ""
	for ctx.Err() == nil {
		rc, err := open()
		if err != nil {
			st.openErrors.Add(1)
			// One line per distinct failure: an unplugged board would otherwise
			// log the same error every second until someone plugs it back in.
			if msg := err.Error(); msg != lastErr && ctx.Err() == nil {
				lastErr = msg
				if errors.Is(err, fs.ErrPermission) {
					log.Error("cannot open the ground unit's port", "err", err,
						"hint", "the device is root:uucp 0660 after every replug: sudo chmod 666 the device, or add yourself to uucp")
				} else {
					log.Warn("cannot open the ground unit's port, retrying", "err", err, "every", cfg.Retry)
				}
			}
			if !sleep(ctx, cfg.Retry) {
				break
			}
			continue
		}
		lastErr = ""
		st.opens.Add(1)
		log.Info("port open", "port", cfg.Port)
		err = pump(ctx, rc, send, st)
		_ = rc.Close()
		if ctx.Err() != nil {
			break
		}
		log.Warn("port lost, reopening", "err", err)
		if !sleep(ctx, cfg.Retry) {
			break
		}
	}
	return nil
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func report(ctx context.Context, every time.Duration, log *slog.Logger, st *stats) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s := st.snapshot()
			log.Info("relay stats", "lines", s.Lines, "forwarded", s.Forwarded, "rejected", s.Rejected,
				"send_errors", s.SendErrors, "opens", s.Opens)
		}
	}
}

// pump reads lines until the port fails. A read blocked on a quiet port is
// ended by closing the port when ctx is cancelled.
func pump(ctx context.Context, rc io.ReadCloser, send func([]byte) error, st *stats) error {
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = rc.Close()
		case <-done:
		}
	}()

	buf := make([]byte, 4096)
	var line []byte
	discarding := false // inside a line already known to be too long
	for {
		n, err := rc.Read(buf)
		chunk := buf[:n]
		for len(chunk) > 0 {
			i := bytes.IndexByte(chunk, '\n')
			if i < 0 {
				if !discarding {
					line = append(line, chunk...)
					if len(line) > litewing.MaxLine {
						discarding, line = true, line[:0]
					}
				}
				break
			}
			if discarding {
				// The over-long line ends here. It still counts, as one
				// rejected line, so a noisy cable shows up in the stats.
				st.lines.Add(1)
				st.rejected.Add(1)
				discarding = false
			} else {
				line = append(line, chunk[:i]...)
				handleLine(line, send, st)
			}
			line = line[:0]
			chunk = chunk[i+1:]
		}
		if err != nil {
			return err
		}
	}
}

func handleLine(line []byte, send func([]byte) error, st *stats) {
	st.lines.Add(1)
	if !litewing.IsLine(line) {
		return // the board's own log output
	}
	if _, err := litewing.Parse(line); err != nil {
		st.rejected.Add(1)
		return
	}
	payload := bytes.TrimRight(bytes.TrimPrefix(line, []byte(litewing.Prefix)), "\r")
	if err := send(payload); err != nil {
		// Nobody listening yet (the stack is down) is normal and transient.
		st.sendErrs.Add(1)
		return
	}
	st.forwarded.Add(1)
}

// udpSender dials the bridge. A connected UDP socket, so a bridge that is not
// up yet shows as a send error (ECONNREFUSED) instead of silence.
func udpSender(to string) (func([]byte) error, func(), error) {
	conn, err := net.Dial("udp", to)
	if err != nil {
		return nil, nil, err
	}
	send := func(b []byte) error {
		_, err := conn.Write(b)
		return err
	}
	return send, func() { _ = conn.Close() }, nil
}

func realMain(ctx context.Context, args []string, stderr io.Writer) int {
	log := slog.New(slog.NewTextHandler(stderr, nil))
	cfg, err := parseFlags(args, stderr)
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stderr, "litewing-relay:", err)
		}
		return exitConfig
	}
	send, closeSend, err := udpSender(cfg.To)
	if err != nil {
		log.Error("cannot reach the bridge address", "to", cfg.To, "err", err)
		return exitRuntime
	}
	defer closeSend()
	log.Info("relaying", "port", cfg.Port, "baud", cfg.Baud, "to", cfg.To)
	st := &stats{}
	open := func() (io.ReadCloser, error) { return openSerial(cfg.Port, cfg.Baud) }
	_ = run(ctx, cfg, open, send, log, st)
	s := st.snapshot()
	log.Info("stopped", "forwarded", s.Forwarded, "rejected", s.Rejected, "send_errors", s.SendErrors)
	return exitOK
}

// signalContext ends on Ctrl-C or SIGTERM.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func main() { os.Exit(withSignals(os.Args[1:], os.Stderr)) }

func withSignals(args []string, stderr io.Writer) int {
	ctx, stop := signalContext()
	defer stop()
	return realMain(ctx, args, stderr)
}
