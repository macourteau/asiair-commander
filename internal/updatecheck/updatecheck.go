// Package updatecheck asks GitHub, about once an hour, whether a release newer
// than the running build has been published, so the web UI can point the
// operator at it (design features §7). It is advisory only: it never downloads
// or installs anything, it keeps what it learned in memory, and any failure
// leaves the last good answer in place.
package updatecheck

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/semver"
)

// DefaultBaseURL is the GitHub REST API root.
const DefaultBaseURL = "https://api.github.com"

// latestReleasePath is the repository's "latest release" endpoint. GitHub
// excludes drafts and prereleases from it, so the astap-cli-* mirror
// prereleases can never be reported as an application update.
const latestReleasePath = "/repos/macourteau/asiair-commander/releases/latest"

const (
	// requestTimeout bounds one whole check, connect to last byte.
	requestTimeout = 15 * time.Second
	// maxBodyBytes bounds the response read. A release object carries its notes
	// and asset list, which stay far below this; a body that does not fit fails
	// to decode and is treated like any other failed check.
	maxBodyBytes = 1 << 20

	// The first check lands 1–10 min after startup so a fleet restarted together
	// (a host reboot, a watchtower pull) does not query at the same instant, and
	// so a crash-looping process does not spend the unauthenticated rate limit.
	firstDelayMin = 1 * time.Minute
	firstDelayMax = 10 * time.Minute
	// Later checks run every interval ± intervalJitter.
	interval       = 60 * time.Minute
	intervalJitter = 5 * time.Minute
)

// Options configures a Checker. Version and Enabled are required.
type Options struct {
	// Version is the running build's version (main.version).
	Version string
	// Enabled reports whether checking is switched on, read at every tick and
	// every Latest call so a config change applies without a restart.
	Enabled func() bool
	// OnChange, when set, is called after a check changes what Latest reports.
	OnChange func()
	Logger   *slog.Logger

	// BaseURL overrides DefaultBaseURL. Tests and the demo build point it at a
	// local fake; production never sets it.
	BaseURL string
	// Client overrides the HTTP client (default: one with requestTimeout).
	Client *http.Client
	// Int64N returns a uniform value in [0, n) and drives the schedule jitter.
	// nil uses math/rand/v2.
	Int64N func(n int64) int64
	// After is the scheduler's timer. nil uses time.After.
	After func(time.Duration) <-chan time.Time
}

// Checker periodically fetches the latest published release.
type Checker struct {
	opts    Options
	current string // canonical running version; "" when it is not a release
	log     *slog.Logger

	mu     sync.Mutex
	latest string // newest release tag seen that is semver-greater than current
}

// New builds a Checker. It does no I/O; Run starts the schedule.
func New(opts Options) *Checker {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.BaseURL == "" {
		opts.BaseURL = DefaultBaseURL
	}
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: requestTimeout}
	}
	if opts.Int64N == nil {
		opts.Int64N = rand.Int64N
	}
	if opts.After == nil {
		opts.After = time.After
	}
	current, _ := releaseVersion(opts.Version)
	return &Checker{opts: opts, current: current, log: opts.Logger}
}

// Latest returns the newest published release tag that is newer than the
// running build, or "" when there is none, none is known yet, or checking is
// switched off. The value survives being switched off and on again: it is still
// a release that was published, so turning the check back on shows it at once.
func (c *Checker) Latest() string {
	if !c.opts.Enabled() {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.latest
}

// Run checks on the schedule until ctx ends. A build that is not a clean
// release (dev, untagged, a git-describe suffix) never checks: it has no
// release to be behind, and a developer build must not call out.
func (c *Checker) Run(ctx context.Context) {
	if c.current == "" {
		c.log.Debug("updatecheck: not a release build, never checking", "version", c.opts.Version)
		return
	}
	delay := c.firstDelay()
	c.log.Debug("updatecheck: scheduled", "version", c.current, "first_check_in", delay)
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.opts.After(delay):
		}
		if c.opts.Enabled() {
			c.checkOnce(ctx)
		} else {
			c.log.Debug("updatecheck: switched off, skipping this check")
		}
		delay = c.nextDelay()
	}
}

// firstDelay is uniform in [firstDelayMin, firstDelayMax].
func (c *Checker) firstDelay() time.Duration {
	span := int64(firstDelayMax - firstDelayMin)
	return firstDelayMin + time.Duration(c.opts.Int64N(span+1))
}

// nextDelay is uniform in [interval-intervalJitter, interval+intervalJitter].
func (c *Checker) nextDelay() time.Duration {
	span := int64(2 * intervalJitter)
	return interval - intervalJitter + time.Duration(c.opts.Int64N(span+1))
}

// checkOnce runs one fetch and records its answer. Every failure is logged and
// otherwise ignored, keeping the last good value: an offline observatory or a
// rate-limited address must not flap the indicator.
func (c *Checker) checkOnce(ctx context.Context) {
	tag, err := c.fetch(ctx)
	if err != nil {
		c.log.Debug("updatecheck: check failed, keeping the last known release", "err", err)
		return
	}
	latest, ok := releaseVersion(tag)
	if !ok {
		c.log.Debug("updatecheck: latest release tag is not a release version, ignoring", "tag", tag)
		return
	}
	var next string
	if semver.Compare(latest, c.current) > 0 {
		next = tag
	}
	c.mu.Lock()
	changed := next != c.latest
	c.latest = next
	c.mu.Unlock()
	c.log.Debug("updatecheck: checked", "running", c.current, "latest", tag, "newer", next != "")
	if !changed {
		return
	}
	if next != "" {
		c.log.Info("a newer release is available", "running", c.current, "latest", next)
	}
	if c.opts.OnChange != nil {
		c.opts.OnChange()
	}
}

// fetch returns the tag_name of the latest published release.
func (c *Checker) fetch(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.opts.BaseURL, "/")+latestReleasePath, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "asiair-commander/"+c.opts.Version)
	resp, err := c.opts.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Drain a little so the connection can be reused; the status says enough.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return "", fmt.Errorf("unexpected status %s", resp.Status)
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(&body); err != nil {
		return "", fmt.Errorf("decode release: %w", err)
	}
	if body.TagName == "" {
		return "", fmt.Errorf("release has no tag_name")
	}
	return body.TagName, nil
}

// releaseVersion returns v in canonical "vMAJOR.MINOR.PATCH" form when it names
// a clean release, and false for anything else: "dev", "", a bare commit hash,
// a git-describe build between tags ("v1.2.3-4-gabcdef" parses as a
// prerelease), or an abbreviated "v1.2". A missing leading "v" is tolerated.
func releaseVersion(v string) (string, bool) {
	if v != "" && v[0] != 'v' {
		v = "v" + v
	}
	if !semver.IsValid(v) || semver.Canonical(v) != v || semver.Prerelease(v) != "" {
		return "", false
	}
	return v, true
}
