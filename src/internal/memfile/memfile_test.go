package memfile

import (
	"io"
	"os"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestAMemfileHasNoPath(t *testing.T) {
	f, err := New("x", []byte("abc"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = f.Close() }()
	fd := int(f.Fd())

	link, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(link, "/memfd:x") {
		t.Errorf("the descriptor points at %q, want /memfd:x", link)
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Error("the descriptor is inherited by every process the hub starts, want close-on-exec")
	}
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "abc" {
		t.Errorf("read %q, want abc", data)
	}
}
