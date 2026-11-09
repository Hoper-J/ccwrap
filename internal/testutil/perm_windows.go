package testutil

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

// MakeWritableByOthers lets Everyone change dir, the Windows counterpart of
// chmod 0777. (internal/app's own tests keep a copy: this package imports
// app.)
func MakeWritableByOthers(t testing.TB, dir string) {
	t.Helper()
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + u.User.Sid.String() + ")(A;OICI;0x1301bf;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

// Symlink makes newname a symlink to oldname, skipping the test where this
// user may not create symlinks (no Developer Mode and not elevated).
func Symlink(t testing.TB, oldname, newname string) {
	t.Helper()
	if err := os.Symlink(oldname, newname); err != nil {
		if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
			t.Skipf("creating a symlink needs Developer Mode or an elevated prompt: %v", err)
		}
		t.Fatal(err)
	}
}
