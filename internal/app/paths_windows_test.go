package app

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

const (
	wantWritableByOthers = "others can write to it ("
	wantOwnedBy          = "it is owned by "
)

func selfSID(t *testing.T) *windows.SID {
	t.Helper()
	sid, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	return sid
}

// setDACL gives dir the access list in SDDL dacl, not inherited.
func setDACL(t *testing.T, dir, dacl string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(dacl)
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
}

// makeWritableByOthers lets Everyone change dir, like chmod 0777.
func makeWritableByOthers(t *testing.T, dir string) {
	setDACL(t, dir, "D:P(A;OICI;FA;;;"+selfSID(t).String()+")(A;OICI;0x1301bf;;;WD)")
}

// makeReadableByOthers lets Everyone read dir, like chmod 0755.
func makeReadableByOthers(t *testing.T, dir string) {
	setDACL(t, dir, "D:P(A;OICI;FA;;;"+selfSID(t).String()+")(A;OICI;0x1200a9;;;WD)")
}

// pretendOwnedBySomeoneElse makes the current user dir's owner (an elevated
// process's directories belong to Administrators, which is trusted) and has
// ownership checks see another user as the current one.
func pretendOwnedBySomeoneElse(t *testing.T, dir string) {
	t.Helper()
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION, selfSID(t), nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	other, err := windows.StringToSid("S-1-5-21-1-2-3-1001")
	if err != nil {
		t.Fatal(err)
	}
	prev := currentUserSID
	currentUserSID = func() (*windows.SID, error) { return other, nil }
	t.Cleanup(func() { currentUserSID = prev })
}

// permSnapshot is dir's owner and access list, in SDDL.
func permSnapshot(t *testing.T, dir string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	return sd.String()
}

func assertOwnerOnly(t *testing.T, dir string) {
	t.Helper()
	if problem := othersCanWrite(dir, nil); problem != "" {
		t.Fatalf("runtime dir: %s; want only its owner able to write", problem)
	}
}

// symlink skips the test where this user may not create symlinks (no
// Developer Mode and not elevated).
func symlink(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Symlink(oldname, newname); err != nil {
		if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
			t.Skipf("creating a symlink needs Developer Mode or an elevated prompt: %v", err)
		}
		t.Fatal(err)
	}
}
