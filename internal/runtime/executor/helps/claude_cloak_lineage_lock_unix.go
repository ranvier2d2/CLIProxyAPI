//go:build unix

package helps

import (
	"golang.org/x/sys/unix"
)

// lockClaudeCloakLineageShard takes the exclusive lock that serializes
// writers of key's Session across processes sharing dir. The kernel releases
// it if the process dies.
func lockClaudeCloakLineageShard(dir, key string) (func(), error) {
	file, errOpen := openClaudeCloakLineageShard(dir, key)
	if errOpen != nil {
		return nil, errOpen
	}
	for {
		errLock := unix.Flock(int(file.Fd()), unix.LOCK_EX)
		if errLock == nil {
			break
		}
		if errLock != unix.EINTR {
			_ = file.Close()
			return nil, errLock
		}
	}
	return func() {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
	}, nil
}
