// Package uuurun extracts the embedded stock uuu binary and runs it,
// translating uuu's verbose console output into structured events.
package uuurun

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Event is one parsed line of uuu -v output.
type Event struct {
	Type    string // attach, cmdstart, cmdok, cmdfail, progress, wait, info, line
	Dev     string // usb path like "1:16" when known
	Text    string
	Percent int
}

// Device is one row of `uuu -lsusb`.
type Device struct {
	Path   string `json:"path"`
	Chip   string `json:"chip"`
	Pro    string `json:"pro"`
	Vid    string `json:"vid"`
	Pid    string `json:"pid"`
	Serial string `json:"serial"`
}

// Extract writes the embedded uuu binary to the user cache dir (keyed by
// content hash so upgrades never collide) and returns its path.
func Extract(bin []byte, exeName string) (string, error) {
	if len(bin) == 0 {
		return "", fmt.Errorf("no embedded uuu binary for this platform")
	}
	sum := sha256.Sum256(bin)
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	dir = filepath.Join(dir, "uuu-gui")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := exeName
	ext := filepath.Ext(exeName)
	base := strings.TrimSuffix(name, ext)
	path := filepath.Join(dir, base+"-"+hex.EncodeToString(sum[:6])+ext)

	if fi, err := os.Stat(path); err == nil && fi.Size() == int64(len(bin)) {
		return path, nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, bin, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}

// IsPrivileged reports whether uuu can already claim USB devices without
// further setup (used to decide whether a password prompt is coming).
func IsPrivileged(exe string) bool {
	switch runtime.GOOS {
	case "darwin", "linux":
		return os.Geteuid() == 0 || isSetuidRoot(exe)
	default:
		return true
	}
}

// EnsurePrivileged makes sure the extracted uuu binary can claim USB devices.
//
// On macOS detaching the HID kernel driver needs root (libusb error -3
// otherwise), so the cached uuu is made setuid-root once via the native
// administrator-password dialog; later runs need no prompt. The setuid bit
// only changes the effective uid, so this process can still kill uuu to
// cancel a flash. On Linux root (sudo) or udev rules are required up front.
func EnsurePrivileged(exe string) error {
	switch runtime.GOOS {
	case "darwin":
		if os.Geteuid() == 0 || isSetuidRoot(exe) {
			return nil
		}
		script := fmt.Sprintf(
			`do shell script "/usr/sbin/chown root:wheel '%s' && /bin/chmod 4755 '%s'" `+
				`with administrator privileges `+
				`with prompt "Board Flasher needs administrator access to talk to the board over USB."`,
			exe, exe)
		out, err := exec.Command("/usr/bin/osascript", "-e", script).CombinedOutput()
		if err != nil {
			return fmt.Errorf("administrator access not granted: %s", strings.TrimSpace(string(out)))
		}
		if !isSetuidRoot(exe) {
			return fmt.Errorf("could not make uuu privileged")
		}
		return nil
	case "linux":
		if os.Geteuid() != 0 && !isSetuidRoot(exe) {
			return fmt.Errorf("USB access needs root: restart with sudo, or install uuu udev rules")
		}
		return nil
	default:
		return nil
	}
}

func isSetuidRoot(exe string) bool {
	fi, err := os.Stat(exe)
	if err != nil {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Uid == 0 && fi.Mode()&os.ModeSetuid != 0
}

// Version returns the uuu version banner line.
func Version(exe string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, exe).Output()
	if i := bytes.IndexByte(out, '\n'); i > 0 {
		return strings.TrimSpace(string(out[:i]))
	}
	return ""
}

// ListDevices runs `uuu -lsusb` and parses the device table.
func ListDevices(exe string) ([]Device, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, exe, "-lsusb").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("uuu -lsusb: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return parseLsusb(string(out)), nil
}

func parseLsusb(out string) []Device {
	devs := []Device{}
	sawSep := false
	for _, line := range strings.Split(stripANSI(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "====") {
			sawSep = true
			continue
		}
		if !sawSep || line == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 5 || !strings.HasPrefix(f[3], "0x") {
			continue
		}
		d := Device{Path: f[0], Chip: f[1], Pro: strings.TrimSuffix(f[2], ":"),
			Vid: f[3], Pid: f[4]}
		if len(f) >= 7 {
			d.Serial = f[6]
		}
		devs = append(devs, d)
	}
	return devs
}

// Run executes uuu with args, streaming parsed events to fn. It returns nil
// when uuu exits 0. Cancel ctx to abort the flash.
func Run(ctx context.Context, exe string, args []string, fn func(Event)) error {
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.WaitDelay = 3 * time.Second

	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		pw.Close()
		return err
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		sc.Split(scanConsoleLines)
		p := &parser{fn: fn}
		for sc.Scan() {
			p.line(sc.Text())
		}
	}()

	err := cmd.Wait()
	pw.Close()
	<-done
	return err
}

// scanConsoleLines splits on \n or \r so in-place progress updates
// ("\r42%") become separate tokens.
func scanConsoleLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

var (
	ansiRe     = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
	attachRe   = regexp.MustCompile(`^New USB Device Attached at (\S+)`)
	cmdStartRe = regexp.MustCompile(`^(\S+)>Start Cmd:\s*(.*)$`)
	cmdOkRe    = regexp.MustCompile(`^(\S+)>Okay \(([0-9.]+)s\)`)
	cmdFailRe  = regexp.MustCompile(`^(\S+)>Fail (.*)$`)
	pctRe      = regexp.MustCompile(`^([0-9]{1,3})%$`)
	waitRe     = regexp.MustCompile(`^Wait for [^.]*`)
)

func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

type parser struct {
	fn      func(Event)
	lastDev string
}

func (p *parser) line(raw string) {
	s := strings.TrimSpace(stripANSI(raw))
	if s == "" {
		return
	}
	switch {
	case attachRe.MatchString(s):
		m := attachRe.FindStringSubmatch(s)
		p.lastDev = m[1]
		p.fn(Event{Type: "attach", Dev: m[1], Text: s})
	case cmdStartRe.MatchString(s):
		m := cmdStartRe.FindStringSubmatch(s)
		p.lastDev = m[1]
		p.fn(Event{Type: "cmdstart", Dev: m[1], Text: m[2]})
	case cmdOkRe.MatchString(s):
		m := cmdOkRe.FindStringSubmatch(s)
		p.fn(Event{Type: "cmdok", Dev: m[1], Text: s})
	case cmdFailRe.MatchString(s):
		m := cmdFailRe.FindStringSubmatch(s)
		p.fn(Event{Type: "cmdfail", Dev: m[1], Text: m[2]})
	case pctRe.MatchString(s):
		m := pctRe.FindStringSubmatch(s)
		n, _ := strconv.Atoi(m[1])
		if n >= 0 && n <= 100 {
			p.fn(Event{Type: "progress", Dev: p.lastDev, Percent: n})
		}
	case waitRe.MatchString(s):
		p.fn(Event{Type: "wait", Text: strings.TrimRight(s, " -\\|/")})
	default:
		p.fn(Event{Type: "line", Text: s})
	}
}
