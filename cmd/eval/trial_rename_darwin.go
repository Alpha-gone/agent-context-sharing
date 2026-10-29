package main

import "golang.org/x/sys/unix"

func renameTrialExclusive(from, to string) error { return unix.RenamexNp(from, to, unix.RENAME_EXCL) }
