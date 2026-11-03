package rig

import (
	"crypto/sha256"
	"testing"

	"github.com/macourteau/asiair-commander/internal/header"
)

// corruptingWriter wraps the real writer: it makes an honest edit, then hands the
// output bytes to mutate and writes back whatever mutate returns, reporting the
// output's true length and hash so the at-rest read-back agrees with it. The proof
// digests stay the honest writer's — which agree — unless forge rewrites them, so
// a test can model a writer that corrupts and still claims to be faithful.
func corruptingWriter(t *testing.T, mutate func(t *testing.T, b []byte) []byte, forge func(*header.EditResult)) func(src, dst string, edits []header.CardEdit) (header.EditResult, error) {
	return func(src, dst string, edits []header.CardEdit) (header.EditResult, error) {
		er, err := header.EditCards(src, dst, edits)
		if err != nil {
			return er, err
		}
		b := mutate(t, readLocal(t, dst))
		writeLocal(t, dst, b)
		er.OutputSize = int64(len(b))
		er.OutputSHA256 = sha256.Sum256(b)
		if forge != nil {
			forge(&er)
		}
		return er, nil
	}
}

// flipLastByte corrupts the final byte, which lives in the data unit.
func flipLastByte(_ *testing.T, b []byte) []byte {
	b[len(b)-1] ^= 0xFF
	return b
}
