//go:build darwin

package imagebridge

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

func unixPeerUID(connection *net.UnixConn) (int, error) {
	raw, err := connection.SyscallConn()
	if err != nil {
		return 0, err
	}

	var (
		credential *unix.Xucred
		controlErr error
	)

	err = raw.Control(func(fd uintptr) {
		credential, controlErr = unix.GetsockoptXucred(
			int(fd),
			unix.SOL_LOCAL,
			unix.LOCAL_PEERCRED,
		)
	})
	if err != nil || controlErr != nil || credential == nil {
		return 0, fmt.Errorf("%w: read peer credentials", errorsJoin(err, controlErr))
	}

	return int(credential.Uid), nil
}
