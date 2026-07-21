//go:build darwin || linux

package cli

import (
	"context"
	"errors"
	"math"
	"os"

	"golang.org/x/sys/unix"
)

const inputPollMillis = 50

func readFileBounded(ctx context.Context, file *os.File, maximum int64) ([]byte, error) {
	fd, restore, err := prepareNonblockingFile(file)
	if err != nil {
		return nil, err
	}
	defer restore()

	return readNonblockingFile(ctx, fd, maximum)
}

func prepareNonblockingFile(file *os.File) (int32, func(), error) {
	rawFD := file.Fd()
	if rawFD > math.MaxInt32 {
		return 0, nil, unix.EBADF
	}

	fd := int(rawFD)

	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		return 0, nil, err
	}

	if flags&unix.O_NONBLOCK != 0 {
		return int32(rawFD), func() {}, nil
	}

	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFL, flags|unix.O_NONBLOCK); err != nil {
		return 0, nil, err
	}

	restore := func() { _, _ = unix.FcntlInt(uintptr(fd), unix.F_SETFL, flags) }

	return int32(rawFD), restore, nil
}

func readNonblockingFile(ctx context.Context, fd int32, maximum int64) ([]byte, error) {
	data := make([]byte, 0, min(maximum, 32<<10))
	buffer := make([]byte, 32<<10)

	for int64(len(data)) < maximum {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		remaining := maximum - int64(len(data))

		chunk := buffer
		if remaining < int64(len(chunk)) {
			chunk = chunk[:remaining]
		}

		count, eof, wait, readErr := readNonblockingChunk(int(fd), chunk)
		if count > 0 {
			data = append(data, chunk[:count]...)
			continue
		}

		if eof {
			return data, nil
		}

		if readErr != nil {
			return nil, readErr
		}

		if wait {
			if err := pollInput(fd); err != nil {
				return nil, err
			}
		}
	}

	return data, nil
}

func readNonblockingChunk(fd int, chunk []byte) (int, bool, bool, error) {
	count, err := unix.Read(fd, chunk)
	if count > 0 {
		return count, false, false, nil
	}

	if err == nil {
		return 0, true, false, nil
	}

	if errors.Is(err, unix.EINTR) {
		return 0, false, false, nil
	}

	if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
		return 0, false, true, nil
	}

	return 0, false, false, err
}

func pollInput(fd int32) error {
	poll := []unix.PollFd{{Fd: fd, Events: unix.POLLIN | unix.POLLHUP | unix.POLLERR}}
	if _, err := unix.Poll(poll, inputPollMillis); err != nil && !errors.Is(err, unix.EINTR) {
		return err
	}

	return nil
}
