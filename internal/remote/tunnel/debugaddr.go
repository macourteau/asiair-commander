package tunnel

import (
	"fmt"
	"os"
	"strconv"
)

// DebugAddrVar is tailcat's knob that logs a tunnel address, a secret bearer
// capability carrying the PSK, through the standard logger, past every
// redaction.
const DebugAddrVar = "TS_DEBUG_ADDR"

// CheckDebugAddr refuses while DebugAddrVar is on, read through getenv
// (os.Getenv when nil). A value that does not parse as a boolean is refused
// too, since tailcat may read it as on. Both ends share it: the remote role
// before it serves, and Commander's Mint and NewClient before they touch a
// key.
func CheckDebugAddr(getenv func(string) string) error {
	if getenv == nil {
		getenv = os.Getenv
	}
	v := getenv(DebugAddrVar)
	if v == "" {
		return nil
	}
	if on, err := strconv.ParseBool(v); err == nil && !on {
		return nil
	}
	return fmt.Errorf("tunnel: %s is set: tailcat would print the tunnel's address, a secret, to the log; unset it", DebugAddrVar)
}
