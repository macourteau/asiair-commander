package mediaid

import (
	"io/fs"
	"path"
)

// fakeOrigin is an in-memory Origin: a map keyed by marker name, with
// per-name injectable read/write errors so tests can simulate an unreadable
// or unwritable share without any real filesystem or SMB dependency.
type fakeOrigin struct {
	data     map[string][]byte
	readErr  map[string]error
	writeErr map[string]error
}

func newFakeOrigin() *fakeOrigin {
	return &fakeOrigin{
		data:     map[string][]byte{},
		readErr:  map[string]error{},
		writeErr: map[string]error{},
	}
}

func (f *fakeOrigin) ReadMarker(name string) ([]byte, error) {
	if err := f.readErr[name]; err != nil {
		return nil, err
	}
	data, ok := f.data[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return data, nil
}

func (f *fakeOrigin) WriteMarker(name string, data []byte) error {
	if err := f.writeErr[name]; err != nil {
		return err
	}
	f.data[name] = append([]byte(nil), data...)
	return nil
}

// fakeMarkerStore is an in-memory MarkerStore keyed by the joined subpath, so a
// test can prove PrefixedOrigin roots access at its prefix. It mirrors the real
// device.Share's path.Join keying.
type fakeMarkerStore struct {
	data map[string][]byte
}

func newFakeMarkerStore() *fakeMarkerStore {
	return &fakeMarkerStore{data: map[string][]byte{}}
}

func (s *fakeMarkerStore) ReadMarker(name string) ([]byte, error) {
	data, ok := s.data[path.Clean(name)]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return data, nil
}

func (s *fakeMarkerStore) WriteMarkerAt(dir, name string, data []byte) error {
	s.data[path.Join(dir, name)] = append([]byte(nil), data...)
	return nil
}
