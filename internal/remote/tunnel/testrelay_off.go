//go:build !remotetest

package tunnel

// testRelayAllowed is false in every shipped build: ParseEnv, NewClient and
// Fingerprint refuse a pairing whose relay region is a test relay, whose TLS
// certificate tailcat never verifies. The remotetest build tag flips it
// (testrelay_on.go) for the cross-version test's binaries and the integration
// tests only.
const testRelayAllowed = false
