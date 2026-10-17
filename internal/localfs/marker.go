package localfs

import (
	"path/filepath"

	"github.com/macourteau/asiair-commander/internal/mediaid"
)

// MarkerOrigin returns a mediaid.Origin that reads and writes a marker file at
// root on the local (NAS destination) filesystem. It joins root and the marker
// name with filepath.Join — a native host path — deliberately NOT reusing
// mediaid.PrefixedOrigin, which joins with path.Join (forward slashes) as is
// correct for an SMB device.Share but wrong for a local Windows path. ReadMarker
// surfaces fs.ErrNotExist unwrapped when the file is absent, so mediaid.Verify can
// distinguish AbsentMarker ("not linked") from Unreadable ("marker unreadable").
func MarkerOrigin(fsys LocalFS, root string) mediaid.Origin {
	return markerOrigin{fsys: fsys, root: root}
}

type markerOrigin struct {
	fsys LocalFS
	root string
}

func (m markerOrigin) ReadMarker(name string) ([]byte, error) {
	return m.fsys.ReadFile(filepath.Join(m.root, name))
}

func (m markerOrigin) WriteMarker(name string, data []byte) error {
	return m.fsys.WriteFileAtomic(filepath.Join(m.root, name), data)
}
