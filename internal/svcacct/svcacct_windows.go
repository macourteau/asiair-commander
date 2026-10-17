//go:build windows

package svcacct

import "golang.org/x/sys/windows"

// serviceAccountName resolves the process token's user SID to a DOMAIN\Name
// string via LookupAccountSid. For the virtual service account this yields
// "NT SERVICE\ASIAIRCommander" — the exact name an operator must reference in an
// icacls/Properties grant. On any failure it degrades to the raw SID string, or a
// generic label, so the caller always has a non-empty identity to display.
func serviceAccountName() string {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return "the ASIAIR Commander service account"
	}
	defer token.Close()

	tu, err := token.GetTokenUser()
	if err != nil {
		return "the ASIAIR Commander service account"
	}

	account, domain, _, err := tu.User.Sid.LookupAccount("")
	if err != nil {
		return tu.User.Sid.String()
	}
	if domain == "" {
		return account
	}
	return domain + `\` + account
}
