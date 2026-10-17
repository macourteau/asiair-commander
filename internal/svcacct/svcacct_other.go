//go:build !windows

package svcacct

import "os/user"

// serviceAccountName resolves the current OS user. On the dev platforms
// (darwin/linux) there is no virtual service account, so the operator's own
// username is the meaningful identity.
func serviceAccountName() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "the account running ASIAIR Commander"
}
