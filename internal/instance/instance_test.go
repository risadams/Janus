package instance

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestASecondJanusOnTheSameFolderIsTurnedAway(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	first, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	if err := first.Describe("pid 4242, listening on http://127.0.0.1:8787"); err != nil {
		t.Fatal(err)
	}

	second, err := Acquire(dir)
	var held *HeldError
	if !errors.As(err, &held) {
		if second != nil {
			second.Release()
		}
		t.Fatalf("second Acquire = %v, want a HeldError", err)
	}
	if held.Holder != "pid 4242, listening on http://127.0.0.1:8787" || held.Dir != dir {
		t.Errorf("held = %+v", held)
	}
	want := "another janus (pid 4242, listening on http://127.0.0.1:8787) is already using the log folder " + dir
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

func TestTheLockIsFreeOnceReleased(t *testing.T) {
	dir := t.TempDir()
	first, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	first.Describe("pid 1, the first")
	first.Release()

	second, err := Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire after Release = %v", err)
	}
	defer second.Release()
	// What the first wrote is replaced, not added to.
	second.Describe("pid 2")
	if data, _ := os.ReadFile(filepath.Join(dir, FileName)); string(data) != "pid 2\n" {
		t.Errorf("lock file = %q", data)
	}
}

func TestEachFolderHasItsOwnLock(t *testing.T) {
	a, err := Acquire(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	b, err := Acquire(t.TempDir())
	if err != nil {
		t.Fatalf("a second folder could not be locked: %v", err)
	}
	b.Release()
}

func TestAHolderThatHasNotSaidWhoItIsIsStillReported(t *testing.T) {
	dir := t.TempDir()
	first, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	_, err = Acquire(dir)
	if err == nil || err.Error() != "another janus is already using the log folder "+dir {
		t.Errorf("error = %v", err)
	}
}

func TestAddrInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, err = net.Listen("tcp", ln.Addr().String())
	if err == nil || !AddrInUse(err) {
		t.Errorf("AddrInUse(%v) = false", err)
	}
	if AddrInUse(errors.New("something else")) {
		t.Error("AddrInUse matched an unrelated error")
	}
}
