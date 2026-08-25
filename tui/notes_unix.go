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

// openDirNoFollow opens a directory refusing a symlink at its final component.
//
// Lector reproduced the reason: replacing `<base>/ws-1` with a symlink to an
// outside directory made a path-based Remove delete a file outside the tree
// entirely. Resolving the path with EvalSymlinks and comparing prefixes would
// still race — the check and the unlink would address the name twice. Holding a
// descriptor to the directory and unlinking RELATIVE to it addresses it once.
func openDirNoFollow(dir string) (*os.File, error) {
	d, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	st, err := d.Stat()
	if err != nil {
		d.Close()
		return nil, err
	}
	if !st.IsDir() {
		d.Close()
		return nil, fmt.Errorf("tui: %s is not a directory", dir)
	}
	return d, nil
}

// removeAt unlinks name inside dir, refusing to follow a symlinked dir, and
// fsyncs the directory so the removal is durable.
//
// The two failures are reported DIFFERENTLY on purpose. A failed unlink means
// the file is still there and a retry is safe. A failed fsync happens AFTER the
// unlink, so the file may already be gone: that is an uncertain partial result,
// not a delete failure, and reporting it as one invites a retry that would act
// on a different file (ADR-0068 criteria 45-47).
func removeAt(dir, name string) (removed bool, err error) {
	d, err := openDirNoFollow(dir)
	if err != nil {
		return false, err
	}
	defer d.Close()
	if err := syscall.Unlinkat(int(d.Fd()), name); err != nil {
		if err == syscall.ENOENT {
			return false, nil
		}
		return false, err
	}
	if serr := d.Sync(); serr != nil {
		return true, fmt.Errorf("tui: %s was removed but the directory could not be "+
			"synced, so the removal may not be durable: %w", name, serr)
	}
	return true, nil
}
