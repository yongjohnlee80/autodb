//go:build !linux

package main

import "errors"

// holdInode has no portable form. O_PATH is a Linux extension, and an ordinary
// open on a bound unix socket fails with ENXIO everywhere.
//
// The caller falls back to comparing the path's stat, which is EXACTLY what
// shipped before the hold existed: sound on a filesystem that does not recycle
// inode numbers, unsound on one that does. Strictly no worse, and the
// alternative — declining to remove our own socket — makes the next launch look
// occupied while nothing is listening.
//
// Not measured on darwin. APFS is documented as allocating inode numbers
// monotonically, which would make the fallback sound there, but this project
// has no darwin host to prove it on and an unverified "probably fine" is the
// shape of claim that put the original defect in.
func holdInode(string) (inodeHold, error) {
	return nil, errors.New("no inode hold on this platform")
}
