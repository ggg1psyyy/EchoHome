// Package dhcpname makes emOS's DHCP client send the Echo's hostname,
// without changing emOS. emOS's init starts busybox udhcpc by path
// (/sbin/udhcpc, a symlink to busybox on the RAM-disk root, emos/init/init.c
// net_main) with no hostname, and restarts it within ~5 s whenever it exits.
// So the symlink is replaced by a wrapper that adds `-x hostname:<name>`, and
// the running client is stopped: init's restart goes through the wrapper,
// and every request and renewal from then on carries the name. The address
// stays meanwhile (emOS's hook acts only on bound and renew). The wrapper is
// run once before it goes in; if no client comes back through it, the
// symlink is restored and init restarts its own. The RAM disk is rebuilt at
// each boot, so this is redone at each start.
package dhcpname

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/wilbowes/EchoMuse/internal/atomicfile"
)

// marker is the wrapper's second line: what tells it from anything else.
const marker = "# echohome: emOS's udhcpc with the Echo's DHCP hostname"

// validName is a DNS label (RFC 1123), lowercase: the only thing written into
// the wrapper, so nothing else can reach the shell.
var validName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ErrNotHere: no emOS udhcpc to wrap (FireOS, or a board on dhcpcd).
var ErrNotHere = errors.New("dhcpname: no emOS udhcpc at the expected path")

// Env is where things are, and what it may do; the zero value of a field
// takes the Echo's.
type Env struct {
	Client  string // the path init starts: /sbin/udhcpc
	Busybox string // what it links to: /sbin/busybox
	Shell   string // the wrapper's interpreter: /system/bin/sh
	Proc    string // /proc
	Kill    func(pid int) error
	Run     func(path string, args ...string) ([]byte, error)
	Wait    time.Duration // for the client to come back: 30 s
	Poll    time.Duration
}

func (e *Env) defaults() {
	if e.Client == "" {
		e.Client = "/sbin/udhcpc"
	}
	if e.Busybox == "" {
		e.Busybox = "/sbin/busybox"
	}
	if e.Shell == "" {
		e.Shell = "/system/bin/sh"
	}
	if e.Proc == "" {
		e.Proc = "/proc"
	}
	if e.Wait == 0 {
		e.Wait = 30 * time.Second
	}
	if e.Poll == 0 {
		e.Poll = 500 * time.Millisecond
	}
}

func wrapper(shell, busybox, name string) string {
	return "#!" + shell + "\n" + marker + "\nexec " + busybox + " udhcpc -x hostname:" + name + " \"$@\"\n"
}

// Apply makes the client send name. nil when it does (or will, the next time
// init starts it: none runs now), ErrNotHere where there is nothing to wrap.
func (e Env) Apply(name string) error {
	e.defaults()
	if !validName.MatchString(name) {
		return fmt.Errorf("dhcpname: %q is not a DNS label", name)
	}
	want := wrapper(e.Shell, e.Busybox, name)
	if fi, err := os.Lstat(e.Client); err != nil {
		return ErrNotHere
	} else if fi.Mode()&os.ModeSymlink != 0 {
		if t, _ := os.Readlink(e.Client); filepath.Base(t) != filepath.Base(e.Busybox) {
			return ErrNotHere
		}
	} else if b, err := os.ReadFile(e.Client); err != nil || !strings.Contains(string(b), marker) {
		return ErrNotHere
	} else if string(b) == want && len(e.clients("hostname:"+name)) > 0 {
		return nil // in place, and the client runs through it
	}

	// Tried from a copy first; then written over the symlink (the rename
	// inside atomicfile.Write replaces the link, not busybox).
	trial := e.Client + ".echohome-trial"
	if err := os.WriteFile(trial, []byte(want), 0o755); err != nil {
		return fmt.Errorf("dhcpname: write the wrapper: %w", err)
	}
	out, _ := e.Run(trial, "--help")
	os.Remove(trial)
	if !strings.Contains(string(out), "udhcpc") {
		return fmt.Errorf("dhcpname: the wrapper does not run udhcpc (%.80q)", out)
	}
	if err := atomicfile.Write(e.Client, []byte(want), 0o755); err != nil {
		// Its directory sync may fail on a RAM disk with the file in place:
		// what counts is whether the wrapper is there.
		if b, rerr := os.ReadFile(e.Client); rerr != nil || string(b) != want {
			return fmt.Errorf("dhcpname: put the wrapper in place: %w", err)
		}
	}
	running := e.clients("")
	if len(running) == 0 {
		return nil // init starts it through the wrapper when it needs one
	}
	for _, pid := range running {
		e.Kill(pid)
	}
	for end := time.Now().Add(e.Wait); time.Now().Before(end); time.Sleep(e.Poll) {
		if len(e.clients("hostname:"+name)) > 0 {
			return nil
		}
	}
	// Nothing came back through it: init's own again. Were init to start
	// the client between the two calls, the start fails and init tries again
	// on its next turn.
	os.Remove(e.Client)
	os.Symlink(e.Busybox, e.Client)
	for _, pid := range e.clients("") {
		e.Kill(pid)
	}
	return errors.New("dhcpname: no DHCP client came back through the wrapper; emOS's restored")
}

// clients are the udhcpc processes on wlan0 whose arguments contain with.
func (e Env) clients(with string) []int {
	dirs, _ := os.ReadDir(e.Proc)
	var out []int
	for _, d := range dirs {
		pid, err := strconv.Atoi(d.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(e.Proc, d.Name(), "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
		udhcpc, wlan := false, false
		for i, a := range args {
			if (i == 0 && filepath.Base(a) == "udhcpc") || (i == 1 && a == "udhcpc") {
				udhcpc = true
			}
			if a == "wlan0" && i > 0 && args[i-1] == "-i" {
				wlan = true
			}
		}
		if udhcpc && wlan && strings.Contains(string(b), with) {
			out = append(out, pid)
		}
	}
	return out
}
