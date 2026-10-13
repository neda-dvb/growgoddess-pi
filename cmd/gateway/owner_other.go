//go:build !unix

package main

// keepOwner: file ownership is a Unix matter; elsewhere there is nothing to keep.
func keepOwner(path, tmp string) error { return nil }
