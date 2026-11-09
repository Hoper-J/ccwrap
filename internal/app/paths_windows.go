package app

import (
	"errors"
	"fmt"
	"io/fs"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows has no mode bits: a directory's owner and access list decide who
// can write to it. Administrators and SYSTEM are trusted the way root is on
// Unix; only they can make themselves the owner of anything, and an
// elevated process's new directories are owned by Administrators.

// currentUserSID is the SID this process runs as; a seam for tests.
var currentUserSID = func() (*windows.SID, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return u.User.Sid.Copy()
}

// Access rights that let a principal add, remove or rename entries in a
// directory, or grant itself those rights.
const (
	fileDeleteChild = 0x40
	writeRights     = windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | fileDeleteChild |
		windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER |
		windows.GENERIC_WRITE | windows.GENERIC_ALL | windows.MAXIMUM_ALLOWED
	accessAllowedCallbackACEType = 0x9
)

// ForeignOwner names the owner of the file or directory at path when that is
// neither the current user nor Administrators or SYSTEM. An owner that
// cannot be read counts as foreign.
func ForeignOwner(path string, _ fs.FileInfo) (string, bool) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Sprintf("an owner that cannot be read (%v)", err), true
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return "an owner that cannot be read", true
	}
	self, err := currentUserSID()
	if err != nil {
		return fmt.Sprintf("an owner that cannot be checked (%v)", err), true
	}
	if owner.Equals(self) || privileged(owner) {
		return "", false
	}
	return sidName(owner), true
}

// othersCanWrite names a principal other than the current user, SYSTEM and
// Administrators that the directory's access list lets write to it, or
// returns "". Entries that only pass on to new files count too: what ccwrap
// writes there would be theirs to change.
func othersCanWrite(dir string, _ fs.FileInfo) string {
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Sprintf("its access list cannot be read (%v)", err)
	}
	dacl, _, err := sd.DACL()
	if errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) || (err == nil && dacl == nil) {
		return "anyone can write to it (it has no access list)"
	}
	if err != nil {
		return fmt.Sprintf("its access list cannot be read (%v)", err)
	}
	self, err := currentUserSID()
	if err != nil {
		return fmt.Sprintf("its access list cannot be checked (%v)", err)
	}
	for i := uint16(0); i < dacl.AceCount; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(i), &ace); err != nil {
			return fmt.Sprintf("its access list cannot be read (%v)", err)
		}
		if t := ace.Header.AceType; t != windows.ACCESS_ALLOWED_ACE_TYPE && t != accessAllowedCallbackACEType {
			continue
		}
		if ace.Mask&writeRights == 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if sid.Equals(self) || privileged(sid) || ownerPlaceholder(sid) {
			continue
		}
		return fmt.Sprintf("others can write to it (%s)", sidName(sid))
	}
	return ""
}

// makePrivate replaces dir's access list with one that only the current
// user, SYSTEM and Administrators are on, and stops it inheriting entries
// from its parent.
func makePrivate(dir string) error {
	self, err := currentUserSID()
	if err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + self.String() + ")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

func privileged(sid *windows.SID) bool {
	return sid.IsWellKnown(windows.WinLocalSystemSid) || sid.IsWellKnown(windows.WinBuiltinAdministratorsSid)
}

// ownerPlaceholder is CREATOR OWNER (S-1-3-0) or OWNER RIGHTS (S-1-3-4):
// entries for whoever owns the object, which is checked on its own.
func ownerPlaceholder(sid *windows.SID) bool {
	s := sid.String()
	return s == "S-1-3-0" || s == "S-1-3-4"
}

// sidName is "DOMAIN\name (S-1-…)", or the bare SID when it has no name.
func sidName(sid *windows.SID) string {
	account, domain, _, err := sid.LookupAccount("")
	if err != nil || account == "" {
		return sid.String()
	}
	if domain != "" {
		account = domain + `\` + account
	}
	return account + " (" + sid.String() + ")"
}
