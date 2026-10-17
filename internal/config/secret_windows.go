//go:build windows

package config

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// wrapSecret protects plaintext with Windows DPAPI at machine scope, so the
// secret decrypts under the LocalSystem service account (not a user profile) and
// a bare file read does not yield it (design architecture §7). Returns the
// wrapped bytes; the caller base64-encodes them for the YAML file.
func wrapSecret(plaintext []byte) ([]byte, error) {
	if len(plaintext) == 0 {
		return nil, nil
	}
	in := windows.DataBlob{Size: uint32(len(plaintext)), Data: &plaintext[0]}
	var out windows.DataBlob
	err := windows.CryptProtectData(&in, nil, nil, 0, nil,
		windows.CRYPTPROTECT_LOCAL_MACHINE|windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	if err != nil {
		return nil, fmt.Errorf("DPAPI protect: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	res := make([]byte, out.Size)
	copy(res, unsafe.Slice(out.Data, out.Size))
	return res, nil
}

// unwrapSecret reverses wrapSecret, decrypting the DPAPI-wrapped bytes.
func unwrapSecret(stored []byte) ([]byte, error) {
	if len(stored) == 0 {
		return nil, nil
	}
	in := windows.DataBlob{Size: uint32(len(stored)), Data: &stored[0]}
	var out windows.DataBlob
	err := windows.CryptUnprotectData(&in, nil, nil, 0, nil,
		windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	if err != nil {
		return nil, fmt.Errorf("DPAPI unprotect: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	res := make([]byte, out.Size)
	copy(res, unsafe.Slice(out.Data, out.Size))
	return res, nil
}

// restrictFile replaces the file's DACL with a protected (inheritance-disabled)
// ACL granting full control to LocalSystem, the Administrators group, and the
// account this process runs as, so %ProgramData%'s world-readable inherited ACE
// does not apply to a file holding the session secret and SMB credentials
// (design architecture §7).
//
// The process account is included because the service runs as the least-privilege
// virtual account NT SERVICE\ASIAIRCommander, not LocalSystem: without an ACE for
// itself it could not read back or rewrite the very config it just wrote. Ordinary
// non-admin users still have no access — the protection is unchanged for them.
func restrictFile(path string) error {
	systemSid, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return fmt.Errorf("LocalSystem SID: %w", err)
	}
	adminSid, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return fmt.Errorf("Administrators SID: %w", err)
	}
	selfSid, err := currentProcessSid()
	if err != nil {
		return err
	}

	var pinner runtime.Pinner
	defer pinner.Unpin()
	pinner.Pin(systemSid)
	pinner.Pin(adminSid)
	pinner.Pin(selfSid)

	entries := []windows.EXPLICIT_ACCESS{
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(systemSid),
			},
		},
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(adminSid),
			},
		},
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(selfSid),
			},
		},
	}

	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return fmt.Errorf("build DACL: %w", err)
	}

	err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil)
	if err != nil {
		return fmt.Errorf("apply DACL to %s: %w", path, err)
	}
	return nil
}

// currentProcessSid returns the SID of the account this process runs as (the
// virtual service account on the NAS), read from the process token. The SID data
// lives in a Go-owned buffer the returned pointer keeps alive, so the token can be
// closed immediately.
func currentProcessSid() (*windows.SID, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, fmt.Errorf("open process token: %w", err)
	}
	defer token.Close()
	tu, err := token.GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("query process token user: %w", err)
	}
	return tu.User.Sid, nil
}
