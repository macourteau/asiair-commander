package web

import (
	"archive/zip"
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/macourteau/asiair-commander/internal/model"
)

// remoteCompose is the compose file a pairing is delivered with. It is a copy of
// deploy/remote/docker-compose.yml, because go:embed cannot reach a parent
// directory and the image build's .dockerignore drops deploy/ (so a Go package
// under deploy/remote would not compile there). TestRemoteComposeMatchesDeploy
// fails when the two drift.
//
//go:embed remote_compose.yml
var remoteCompose string

// remoteRegionsTimeout bounds GET /api/remotes/regions, which fetches the relay
// map and measures the nearest region over the network.
const remoteRegionsTimeout = 30 * time.Second

// remotePairingResponse is the create and re-pair response: the only place the
// env value ever appears. EnvFile is the .env a remote needs, ready to save.
// SetupZip is both files in one base64 zip: a browser drops the leading dot of
// a downloaded ".env", but not of an entry inside a zip.
type remotePairingResponse struct {
	Remote      model.RemoteStatus `json:"remote"`
	EnvValue    string             `json:"env_value"`
	EnvFile     string             `json:"env_file"`
	ComposeFile string             `json:"compose_file"`
	SetupZip    string             `json:"setup_zip"`
	Fingerprint string             `json:"fingerprint"`
}

// remoteSetupDir is the folder the setup zip unpacks to.
const remoteSetupDir = "asiair-remote/"

// remoteSetupZip packs the env and compose files into remoteSetupDir. The .env
// is 0600 because it holds the pairing secret.
func remoteSetupZip(envFile, composeFile string, now time.Time) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range []struct {
		name string
		mode os.FileMode
		body string
	}{
		{remoteSetupDir + ".env", 0o600, envFile},
		{remoteSetupDir + "docker-compose.yml", 0o644, composeFile},
	} {
		h := &zip.FileHeader{Name: f.name, Method: zip.Deflate, Modified: now}
		h.SetMode(f.mode)
		fw, err := zw.CreateHeader(h)
		if err != nil {
			return nil, err
		}
		if _, err := io.WriteString(fw, f.body); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// remotesAvailable answers 503 when this build wired no remote supervisor.
func (s *Server) remotesAvailable(w http.ResponseWriter) bool {
	if s.deps.Remotes == nil {
		writeError(w, http.StatusServiceUnavailable, "remote sites are not available")
		return false
	}
	return true
}

// writeRemoteErr maps a supervisor error. Nothing it returns for an unknown
// remote or a failed pairing is logged with a request body: these routes carry
// secrets in their responses.
func (s *Server) writeRemoteErr(w http.ResponseWriter, op, id string, err error) {
	if s.deps.RemoteNotFound != nil && errors.Is(err, s.deps.RemoteNotFound) {
		writeError(w, http.StatusNotFound, "no such remote")
		return
	}
	if s.deps.RemoteRegionUnknown != nil && errors.Is(err, s.deps.RemoteRegionUnknown) {
		writeError(w, http.StatusBadRequest, "no such relay region")
		return
	}
	if s.deps.RelayUnavailable != nil && errors.Is(err, s.deps.RelayUnavailable) {
		s.log.Warn("web: relay service unreachable", "op", op, "err", err)
		writeError(w, http.StatusBadGateway, "could not reach the relay service; check the internet connection")
		return
	}
	s.log.Error("web: remote "+op+" failed", "remote", id, "err", err)
	writeError(w, http.StatusInternalServerError, "could not "+op+" the remote")
}

func (s *Server) handleListRemotes(w http.ResponseWriter, r *http.Request) {
	if !s.remotesAvailable(w) {
		return
	}
	list := s.deps.Remotes.List()
	if list == nil {
		list = []model.RemoteStatus{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleRemoteRegions(w http.ResponseWriter, r *http.Request) {
	if !s.remotesAvailable(w) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), remoteRegionsTimeout)
	defer cancel()
	regions, err := s.deps.Remotes.Regions(ctx)
	if err != nil {
		s.writeRemoteErr(w, "list regions for", "", err)
		return
	}
	if regions == nil {
		regions = []model.RemoteRegion{}
	}
	writeJSON(w, http.StatusOK, regions)
}

// decodeRemoteBody reads a small JSON body into v, answering 400 on failure.
func decodeRemoteBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := decodeJSON(r.Body, v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

// writePairing answers a create or re-pair. The response carries a secret, so
// no cache or proxy may keep it.
func (s *Server) writePairing(w http.ResponseWriter, status int, p model.RemotePairing) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	z, err := remoteSetupZip(p.EnvFile, remoteCompose, time.Now())
	if err != nil {
		s.log.Error("web: building the remote setup zip failed", "remote", p.Remote.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "could not build the remote's setup files")
		return
	}
	writeJSON(w, status, remotePairingResponse{
		Remote:      p.Remote,
		EnvValue:    p.EnvValue,
		EnvFile:     p.EnvFile,
		ComposeFile: remoteCompose,
		SetupZip:    base64.StdEncoding.EncodeToString(z),
		Fingerprint: p.Remote.Fingerprint,
	})
}

func (s *Server) handleCreateRemote(w http.ResponseWriter, r *http.Request) {
	if !s.remotesAvailable(w) {
		return
	}
	var body struct {
		Name   string `json:"name"`
		Region int    `json:"region"`
	}
	if !decodeRemoteBody(w, r, &body) {
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "a remote needs a name")
		return
	}
	if body.Region < 0 {
		writeError(w, http.StatusBadRequest, "invalid relay region")
		return
	}
	p, err := s.deps.Remotes.Add(r.Context(), name, body.Region)
	if err != nil {
		s.writeRemoteErr(w, "pair", "", err)
		return
	}
	s.log.Info("web: remote added", "remote", p.Remote.ID)
	s.writePairing(w, http.StatusCreated, p)
}

func (s *Server) handleRenameRemote(w http.ResponseWriter, r *http.Request) {
	if !s.remotesAvailable(w) {
		return
	}
	id := r.PathValue("id")
	var body struct {
		Name string `json:"name"`
	}
	if !decodeRemoteBody(w, r, &body) {
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "a remote needs a name")
		return
	}
	if err := s.deps.Remotes.Rename(id, name); err != nil {
		s.writeRemoteErr(w, "rename", id, err)
		return
	}
	s.log.Info("web: remote renamed", "remote", id)
	s.writeRemoteState(w, id)
}

func (s *Server) handleSetRemoteRateLimit(w http.ResponseWriter, r *http.Request) {
	if !s.remotesAvailable(w) {
		return
	}
	id := r.PathValue("id")
	var body struct {
		MBps float64 `json:"mbps"`
	}
	if !decodeRemoteBody(w, r, &body) {
		return
	}
	// NaN fails every comparison, so it is rejected by the inverted test.
	if !(body.MBps >= 0) {
		writeError(w, http.StatusBadRequest, "the bandwidth cap must be 0 (off) or more MB/s")
		return
	}
	if err := s.deps.Remotes.SetRateLimit(id, body.MBps); err != nil {
		s.writeRemoteErr(w, "set the bandwidth cap of", id, err)
		return
	}
	s.log.Info("web: remote rate limit set", "remote", id, "mbps", body.MBps)
	s.writeRemoteState(w, id)
}

// writeRemoteState answers a write with the remote's current state.
func (s *Server) writeRemoteState(w http.ResponseWriter, id string) {
	for _, st := range s.deps.Remotes.List() {
		if st.ID == id {
			writeJSON(w, http.StatusOK, st)
			return
		}
	}
	// Removed between the write and this read.
	writeError(w, http.StatusNotFound, "no such remote")
}

// handleRemoteCommand serves POST /api/remotes/{id}:{action}; repair is the only
// action.
func (s *Server) handleRemoteCommand(w http.ResponseWriter, r *http.Request) {
	id, action, ok := strings.Cut(r.PathValue("idAction"), ":")
	if !ok || id == "" || action == "" {
		writeError(w, http.StatusBadRequest, "expected /api/remotes/{id}:{action}")
		return
	}
	if action != "repair" {
		writeError(w, http.StatusBadRequest, "unknown command: "+action)
		return
	}
	if !s.remotesAvailable(w) {
		return
	}
	var body struct {
		Region int `json:"region"`
	}
	if r.ContentLength != 0 && !decodeRemoteBody(w, r, &body) {
		return
	}
	if body.Region < 0 {
		writeError(w, http.StatusBadRequest, "invalid relay region")
		return
	}
	p, err := s.deps.Remotes.Repair(r.Context(), id, body.Region)
	if err != nil {
		s.writeRemoteErr(w, "make new setup files for", id, err)
		return
	}
	s.log.Info("web: remote re-paired", "remote", id)
	s.writePairing(w, http.StatusOK, p)
}

func (s *Server) handleDeleteRemote(w http.ResponseWriter, r *http.Request) {
	if !s.remotesAvailable(w) {
		return
	}
	id := r.PathValue("id")
	if err := s.deps.Remotes.Remove(id); err != nil {
		s.writeRemoteErr(w, "remove", id, err)
		return
	}
	s.log.Info("web: remote removed", "remote", id)
	w.WriteHeader(http.StatusNoContent)
}
