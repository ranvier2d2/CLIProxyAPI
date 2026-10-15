//go:build windows

package helps

import (
	"golang.org/x/sys/windows"
)

// lockClaudeCloakLineageShard takes the exclusive lock that serializes
// writers of key's Session across processes sharing dir. Windows releases it
// if the process dies.
func lockClaudeCloakLineageShard(dir, key string) (func(), error) {
	file, errOpen := openClaudeCloakLineageShard(dir, key)
	if errOpen != nil {
		return nil, errOpen
	}
	handle := windows.Handle(file.Fd())
	if errLock := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, new(windows.Overlapped)); errLock != nil {
		_ = file.Close()
		return nil, errLock
	}
	return func() {
		_ = windows.UnlockFileEx(handle, 0, 1, 0, new(windows.Overlapped))
		_ = file.Close()
	}, nil
}
