package main

import (
	"fmt"
	"syscall"
)

// holdInode takes an O_PATH reference to the socket's inode.
//
// O_PATH opens the FILE rather than the thing behind it, which is why it works
// on a socket at all: an ordinary os.Open on a bound unix socket fails with
// ENXIO because there is no device to open. The reference keeps the inode
// ALLOCATED, so its number cannot be handed to a successor — which is the
// entire property the shutdown identity check depends on.
//
// It replaces a hard-link "pin" that a reviewer showed cancels itself: the pin
// name was derived from the socket path, so a successor binding the same path
// computed the SAME pin name and removed the predecessor's. An open descriptor
// has no name to collide over and is released by the kernel on process exit,
// including a hard kill — a leftover pin file had neither property.
//
// MEASURED on ext4, with the control that makes it a measurement: with nothing
// held a successor DID receive our recycled inode number, and with this
// descriptor held it did not.
func holdInode(path string) (inodeHold, error) {
	// O_PATH is part of the Linux ABI and is not exported by syscall on every
	// Go version, so the constant is written out.
	const oPath = 0x200000
	fd, err := syscall.Open(path, oPath|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("O_PATH open %s: %w", path, err)
	}
	return fdHold(fd), nil
}

type fdHold int

func (f fdHold) release() { _ = syscall.Close(int(f)) }
