// Package netsite is the network-path abstraction: which network a device
// address belongs to, and how bytes reach a device on that network. An Endpoint
// names a site and an address on it; a Site carries the traffic; a Registry
// maps site ids to sites. Every dial and identity probe goes through the site
// its endpoint names, so an address is never tried on a network it does not
// belong to.
//
// The package is named netsite rather than site because "site" already means
// the observing location (siteLoc, the timezone frames are dated in).
package netsite

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// ID identifies a site. The local site's id is the empty string, so the zero
// Endpoint, and every address that predates sites, names the local network.
type ID string

// Local is the id of the local site: Commander's own network, reached with
// plain sockets.
const Local ID = ""

// siteSep separates the site id from the address in a non-local endpoint's
// string form.
//
// An empty DNS label ("..") is the one thing no resolver can put on the wire, so
// a stored "id..ip" fails closed in an older binary that reads it as a bare
// host: net.ParseIP rejects it, Go's resolver rejects it as not a domain name
// before sending any query, and libc's and Windows' resolvers reject it while
// encoding the query. A bare IP or a hostname never contains it, which is what
// keeps every legacy value parsing as the local site.
const siteSep = ".."

// Endpoint is one device address on one site. IP is normally an IP literal; on
// the local site it may also be a hostname (a legacy Host), which the local
// site resolves at dial time exactly as a bare address always was.
type Endpoint struct {
	Site ID
	IP   string
}

// LocalEndpoint names addr on the local site.
func LocalEndpoint(addr string) Endpoint { return Endpoint{Site: Local, IP: addr} }

// String is the endpoint's stored form. The local site's is the bare address,
// unchanged from before sites existed; any other site's is "id..ip".
func (e Endpoint) String() string {
	if e.Site == Local {
		return e.IP
	}
	return string(e.Site) + siteSep + e.IP
}

// errEmptyEndpoint reports an empty stored form; callers treat a missing value
// separately, so reaching Parse with one is a caller's bug worth naming.
var errEmptyEndpoint = errors.New("netsite: empty endpoint")

// Parse reads an endpoint's stored form. A value without the site separator is
// a legacy bare IP or hostname and names the local site verbatim. A value with
// it must carry a valid site id and an IP literal; anything else is an error,
// so a malformed value is never dialled anywhere.
func Parse(s string) (Endpoint, error) {
	if s == "" {
		return Endpoint{}, errEmptyEndpoint
	}
	id, ip, found := strings.Cut(s, siteSep)
	if !found {
		return LocalEndpoint(s), nil
	}
	if !ValidID(ID(id)) {
		return Endpoint{}, fmt.Errorf("netsite: endpoint %q: invalid site id", s)
	}
	if net.ParseIP(ip) == nil {
		return Endpoint{}, fmt.Errorf("netsite: endpoint %q: address is not an IP literal", s)
	}
	return Endpoint{Site: ID(id), IP: ip}, nil
}

// ValidID reports whether id can name a non-local site: non-empty, ASCII
// letters, digits, '-' and '_' only. Excluding '.' is what keeps the stored
// form's separator unambiguous.
func ValidID(id ID) bool {
	if id == Local {
		return false
	}
	for _, r := range string(id) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}
