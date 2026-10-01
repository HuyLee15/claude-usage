//go:build !unix

package main

// lockConfigDir is a no-op where flock is unavailable; concurrent callers may then race on token refresh.
func lockConfigDir(string) (func(), error) {
	return func() {}, nil
}
