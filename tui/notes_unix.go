//go:build unix

package tui

import (
	"fmt"
	"os"
	"syscall"
)

// openNoFollow opens path refusing symlinks and non-regular files.
func openNoFollow(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("tui: %s is not a regular file", path)
	}
	return f, nil
}

// fileIdentity returns the (device, inode) pair of an open descriptor.
func fileIdentity(f *os.File) (dev, ino uint64, err error) {
	st, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("tui: no stat identity available")
	}
	return uint64(sys.Dev), uint64(sys.Ino), nil
}
