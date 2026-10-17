//go:build !windows

package service

// serviceUserName returns an empty account off Windows, so systemd/launchd (dev)
// run the service under their default identity. The virtual service account is a
// Windows-only concept.
func serviceUserName(string) string { return "" }
