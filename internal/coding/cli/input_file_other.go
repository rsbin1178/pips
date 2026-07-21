//go:build !darwin && !linux

package cli

import (
	"context"
	"io"
	"os"
)

func readFileBounded(ctx context.Context, file *os.File, maximum int64) ([]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(file, maximum))
		done <- result{data: data, err: err}
	}()

	select {
	case value := <-done:
		return value.data, value.err
	case <-ctx.Done():
		_ = file.Close()
		<-done

		return nil, ctx.Err()
	}
}
