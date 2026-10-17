//go:build windows

package service

// serviceUserName returns the least-privilege logon account the service runs as
// on Windows: the per-service virtual account NT SERVICE\<name>. Windows
// auto-creates it (no password, per-service SID) when a service is registered
// with this ServiceStartName, so the SCM install path (kardianos passes an empty
// password) provisions it without any secret to manage. It is far less
// privileged than LocalSystem: a compromise of the network-listening web server
// cannot escalate to full machine control.
func serviceUserName(name string) string {
	return `NT SERVICE\` + name
}
