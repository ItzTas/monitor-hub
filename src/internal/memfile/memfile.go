// Package memfile makes files that live only in memory, with no path on any disk.
package memfile

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// New returns a close-on-exec memfd holding data, positioned at its start.
func New(name string, data []byte) (*os.File, error) {
	fd, err := unix.MemfdCreate(name, unix.MFD_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("create the memory file: %w", err)
	}
	f := os.NewFile(uintptr(fd), name)
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("write the memory file: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("rewind the memory file: %w", err)
	}
	return f, nil
}
