package resource

import (
	"fmt"
	"io/fs"
	"sync"
)

type readBudget struct {
	mu      sync.Mutex
	used    int64
	maximum int64
}

func (b *readBudget) add(size int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if size < 0 || b.used > b.maximum-size {
		return false
	}

	b.used += size

	return true
}

type boundedFS struct {
	fsys    fs.FS
	budget  *readBudget
	maxFile int64
}

func (b boundedFS) Open(name string) (fs.File, error) {
	file, err := b.fsys.Open(name)
	if err != nil {
		return nil, err
	}

	return &boundedFile{
		File:    file,
		budget:  b.budget,
		maxFile: b.maxFile,
		name:    name,
	}, nil
}

type boundedFile struct {
	fs.File
	budget  *readBudget
	maxFile int64
	read    int64
	name    string
}

func (f *boundedFile) Stat() (fs.FileInfo, error) {
	info, err := f.File.Stat()
	if err != nil {
		return nil, err
	}

	return boundedFileInfo{FileInfo: info, maximum: f.maxFile}, nil
}

func (f *boundedFile) Read(buffer []byte) (int, error) {
	count, err := f.File.Read(buffer)
	if int64(count) > f.maxFile-f.read {
		return 0, fmt.Errorf("%w: skill %q exceeds %d bytes", ErrLimitExceeded, f.name, f.maxFile)
	}

	if !f.budget.add(int64(count)) {
		return 0, fmt.Errorf("%w: aggregate Skill content exceeds %d bytes", ErrLimitExceeded, f.budget.maximum)
	}

	f.read += int64(count)

	return count, err
}

func (f *boundedFile) ReadDir(count int) ([]fs.DirEntry, error) {
	directory, ok := f.File.(fs.ReadDirFile)
	if !ok {
		return nil, fmt.Errorf("coding resource: %q does not support directory reads", f.name)
	}

	return directory.ReadDir(count)
}

type boundedFileInfo struct {
	fs.FileInfo
	maximum int64
}

func (i boundedFileInfo) Size() int64 {
	return min(i.FileInfo.Size(), i.maximum)
}

var (
	_ fs.FS          = boundedFS{}
	_ fs.ReadDirFile = (*boundedFile)(nil)
)
