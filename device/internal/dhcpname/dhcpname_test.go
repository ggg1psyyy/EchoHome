package dhcpname

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// echo is a fake Echo: a RAM-disk /sbin and a /proc whose udhcpc, killed,
// comes back as init would start it, through whatever /sbin/udhcpc is then.
type echo struct {
	t       *testing.T
	root    string
	mu      sync.Mutex
	next    int
	kills   int
	respawn bool // init starts it again after a kill
	sendsAs func(client string) []string
}

func newEcho(t *testing.T) (*echo, Env) {
	root := t.TempDir()
	for _, d := range []string{"sbin", "proc"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	os.WriteFile(filepath.Join(root, "sbin", "busybox"), []byte("busybox"), 0o755)
	os.Symlink(filepath.Join(root, "sbin", "busybox"), filepath.Join(root, "sbin", "udhcpc"))
	e := &echo{t: t, root: root, next: 400, respawn: true}
	// What init's restart runs as: the wrapper's exec, or busybox's applet.
	e.sendsAs = func(client string) []string {
		b, _ := os.ReadFile(client)
		if s := string(b); strings.Contains(s, marker) {
			name := s[strings.Index(s, "hostname:")+len("hostname:"):]
			name = name[:strings.Index(name, " ")]
			return []string{"/sbin/busybox", "udhcpc", "-x", "hostname:" + name, "-B", "-f", "-i", "wlan0", "-s", "/tmp/udhcpc.sh"}
		}
		return []string{"/sbin/udhcpc", "-B", "-f", "-i", "wlan0", "-s", "/tmp/udhcpc.sh"}
	}
	e.spawn([]string{"/sbin/udhcpc", "-B", "-f", "-i", "wlan0", "-s", "/tmp/udhcpc.sh"})
	e.spawn([]string{"/system/bin/wpa_supplicant", "-iwlan0"})
	env := Env{
		Client: filepath.Join(root, "sbin", "udhcpc"), Busybox: filepath.Join(root, "sbin", "busybox"),
		Proc: filepath.Join(root, "proc"), Wait: 300 * time.Millisecond, Poll: 10 * time.Millisecond,
		Run: func(path string, _ ...string) ([]byte, error) {
			b, _ := os.ReadFile(path)
			if strings.Contains(string(b), "udhcpc") {
				return []byte("Usage: udhcpc [-fbqRB]"), errors.New("exit status 1")
			}
			return nil, errors.New("exit status 127")
		},
		Kill: e.kill,
	}
	return e, env
}

func (e *echo) spawn(args []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.next++
	d := filepath.Join(e.root, "proc", strconv.Itoa(e.next))
	os.MkdirAll(d, 0o755)
	os.WriteFile(filepath.Join(d, "cmdline"), []byte(strings.Join(args, "\x00")+"\x00"), 0o444)
}

func (e *echo) kill(pid int) error {
	os.RemoveAll(filepath.Join(e.root, "proc", strconv.Itoa(pid)))
	e.mu.Lock()
	e.kills++
	again := e.respawn
	e.mu.Unlock()
	if again {
		e.spawn(e.sendsAs(filepath.Join(e.root, "sbin", "udhcpc")))
	}
	return nil
}

func TestTheClientComesBackSendingTheName(t *testing.T) {
	e, env := newEcho(t)
	if err := env.Apply("kitchen-00lq"); err != nil {
		t.Fatal(err)
	}
	if got := env.clients("hostname:kitchen-00lq"); len(got) != 1 || e.kills != 1 {
		t.Fatalf("clients %v after %d kills", got, e.kills)
	}
	// Again, the same name: nothing to do, nothing stopped.
	if err := env.Apply("kitchen-00lq"); err != nil || e.kills != 1 {
		t.Fatalf("%v, %d kills", err, e.kills)
	}
	// A rename: the wrapper rewritten, the client restarted once more.
	if err := env.Apply("hall-0se7"); err != nil || len(env.clients("hostname:hall-0se7")) != 1 {
		t.Fatalf("%v", err)
	}
}

// Nothing came back through the wrapper: emOS's symlink is put back.
func TestWithoutTheWrapperWorkingEmOSsClientIsRestored(t *testing.T) {
	e, env := newEcho(t)
	e.sendsAs = func(string) []string {
		return []string{"/sbin/udhcpc", "-B", "-f", "-i", "wlan0", "-s", "/tmp/udhcpc.sh"}
	}
	if err := env.Apply("kitchen-00lq"); err == nil {
		t.Fatal("no error with no client sending the name")
	}
	if t2, err := os.Readlink(env.Client); err != nil || t2 != env.Busybox {
		t.Fatalf("not restored: %q %v", t2, err)
	}
	if len(env.clients("")) != 1 {
		t.Fatal("no DHCP client running after the restore")
	}
}

func TestAWrapperThatDoesNotRunIsNeverPutInPlace(t *testing.T) {
	e, env := newEcho(t)
	env.Run = func(string, ...string) ([]byte, error) { return []byte("sh: not found"), errors.New("127") }
	if err := env.Apply("kitchen-00lq"); err == nil {
		t.Fatal("no error")
	}
	if t2, _ := os.Readlink(env.Client); t2 != env.Busybox || e.kills != 0 {
		t.Fatalf("changed: %q, %d kills", t2, e.kills)
	}
}

// Only a DNS label reaches the wrapper: nothing else can get into its shell.
func TestOnlyADNSLabelIsWritten(t *testing.T) {
	_, env := newEcho(t)
	for _, bad := range []string{"", "Kitchen", "-x", "a b", "a;reboot", "a$(id)", "a\"b", "x-", strings.Repeat("a", 64), "kü"} {
		if err := env.Apply(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if t2, _ := os.Readlink(env.Client); t2 != env.Busybox {
		t.Fatal("the client changed")
	}
	if err := env.Apply(strings.Repeat("a", 63)); err != nil {
		t.Fatalf("63 characters: %v", err)
	}
}

func TestNothingToWrapIsNotHere(t *testing.T) {
	_, env := newEcho(t)
	os.Remove(env.Client)
	if err := env.Apply("kitchen-00lq"); !errors.Is(err, ErrNotHere) {
		t.Fatalf("missing: %v", err)
	}
	os.Symlink("/system/bin/dhcpcd", env.Client)
	if err := env.Apply("kitchen-00lq"); !errors.Is(err, ErrNotHere) {
		t.Fatalf("something else: %v", err)
	}
}

// No client running yet (no carrier): the wrapper waits for init's start.
func TestWithNoClientRunningTheWrapperWaitsForInit(t *testing.T) {
	e, env := newEcho(t)
	for _, pid := range env.clients("") {
		os.RemoveAll(filepath.Join(e.root, "proc", strconv.Itoa(pid)))
	}
	if err := env.Apply("kitchen-00lq"); err != nil || e.kills != 0 {
		t.Fatalf("%v, %d kills", err, e.kills)
	}
	if b, _ := os.ReadFile(env.Client); !strings.Contains(string(b), "hostname:kitchen-00lq") {
		t.Fatal("the wrapper is not in place")
	}
}
