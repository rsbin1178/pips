package bundle

import (
	"fmt"
	"io/fs"
	"sync"
)

type readBudget struct {
	mu       sync.Mutex
	used     int64
	maxTotal int64
}

func (b *readBudget) add(n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if n < 0 || b.used > b.maxTotal-n {
		return false
	}

	b.used += n

	return true
}

type boundedFS struct {
	fsys    fs.FS
	budget  *readBudget
	maxFile int64
}

func (b *boundedFS) Open(name string) (fs.File, error) {
	file, err := b.fsys.Open(name)
	if err != nil {
		return nil, err
	}

	return &boundedFile{
		File:    file,
		budget:  b.budget,
		maxFile: b.maxFile,
		path:    name,
	}, nil
}

type boundedFile struct {
	fs.File
	budget  *readBudget
	maxFile int64
	read    int64
	path    string
}

func (f *boundedFile) Stat() (fs.FileInfo, error) {
	info, err := f.File.Stat()
	if err != nil {
		return nil, err
	}

	return boundedFileInfo{FileInfo: info, maximum: f.maxFile}, nil
}

func (f *boundedFile) Read(buffer []byte) (int, error) {
	n, err := f.File.Read(buffer)
	if int64(n) > f.maxFile-f.read {
		return 0, fmt.Errorf("%w: resource %q exceeds %d bytes", ErrLimitExceeded, f.path, f.maxFile)
	}

	if !f.budget.add(int64(n)) {
		return 0, fmt.Errorf("%w: aggregate resources exceed %d bytes", ErrLimitExceeded, f.budget.maxTotal)
	}

	f.read += int64(n)

	return n, err
}

func (f *boundedFile) ReadDir(n int) ([]fs.DirEntry, error) {
	directory, ok := f.File.(fs.ReadDirFile)
	if !ok {
		return nil, fmt.Errorf("bundle: %q does not support directory reads", f.path)
	}

	return directory.ReadDir(n)
}

var (
	_ fs.FS          = (*boundedFS)(nil)
	_ fs.ReadDirFile = (*boundedFile)(nil)
)

type boundedFileInfo struct {
	fs.FileInfo
	maximum int64
}

func (i boundedFileInfo) Size() int64 {
	return min(i.FileInfo.Size(), i.maximum)
}
