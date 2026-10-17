//go:build !windows

package config

import "os"

// wrapSecret is the identity on non-Windows platforms: there is no OS-level
// secret store to wrap into, so the stored form is the plaintext bytes (base64
// applied by the caller). Round-trips with unwrapSecret.
func wrapSecret(plaintext []byte) ([]byte, error) { return plaintext, nil }

// unwrapSecret is the identity on non-Windows platforms.
func unwrapSecret(stored []byte) ([]byte, error) { return stored, nil }

// restrictFile best-effort restricts a file to owner read/write. On Windows the
// build-tagged sibling applies a real ACL (service account + Administrators).
func restrictFile(path string) error { return os.Chmod(path, 0o600) }
