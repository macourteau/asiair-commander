package header

import (
	"fmt"

	"github.com/macourteau/asiair-commander/internal/model"
)

// Fake is an in-memory Reader for other packages' tests: it maps a path to a
// pre-built header. An unmapped path returns an error, mirroring the real
// reader's behaviour for a missing or unreadable file.
type Fake struct {
	Headers map[string]model.Header
}

// ReadHeader returns the mapped header for path, or an error if none is mapped.
func (f Fake) ReadHeader(path string) (model.Header, error) {
	h, ok := f.Headers[path]
	if !ok {
		return model.Header{}, fmt.Errorf("header: no fake header for %q", path)
	}
	return h, nil
}

// static assertion that Fake satisfies the seam.
var _ Reader = Fake{}
