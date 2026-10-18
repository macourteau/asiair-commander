//go:build demo

package main

import "os"

// demoReleasesEnv names a local fake of the GitHub API for the update checker,
// so a UI pass can show the newer-release indicator without asking GitHub. It
// exists only in a `-tags demo` build: the production binary has no way to
// point the checker anywhere but api.github.com.
const demoReleasesEnv = "AC_DEMO_RELEASES_URL"

// demoReleasesURL returns the fake's base URL, or "" for the real API.
func demoReleasesURL() string { return os.Getenv(demoReleasesEnv) }
