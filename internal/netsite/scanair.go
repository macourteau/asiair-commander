package netsite

import "time"

// The scan_air exchange is the UDP discovery protocol the ZWO ASIAIR app uses;
// discovery parses the replies. Its request lives here because the local
// site's broadcast round sends it; its port is ScanAirPort, with the other
// device ports.
const (
	// collectWindow is how long a broadcast round listens for replies after
	// sending before returning what answered.
	collectWindow = 2 * time.Second
	// maxReplies bounds how many datagrams one broadcast round collects, so a
	// reply flood cannot make discovery spin.
	maxReplies = 256
)

// ScanAirRequest is the broadcast (and unicast) scan_air request,
// CRLF-terminated exactly as the ASIAIR app sends it.
var ScanAirRequest = []byte("{\"id\":\"132\",\"method\":\"scan_air\",\"name\":\"asiair-commander\"}\r\n")
