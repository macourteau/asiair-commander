// Package svcacct reports the account identity the current process runs as. On
// the NAS that is the least-privilege virtual service account
// (NT SERVICE\ASIAIRCommander); in dev it is the operator's own user. The web
// layer surfaces this name so the setup wizard can tell the operator exactly which
// account to grant folder access to when a destination root is not writable.
package svcacct

// ServiceAccountName returns the account the process runs as, formatted as the
// platform's canonical "who am I" string: DOMAIN\Name on Windows,
// os/user.Current().Username elsewhere. It never returns an error — on any lookup
// failure it degrades to a non-empty best-effort label so the UI always has
// something to show.
func ServiceAccountName() string { return serviceAccountName() }
