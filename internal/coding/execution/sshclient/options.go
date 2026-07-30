package sshclient

import (
	"context"
	"io"
)

// ImageClipboard is the local, push-only clipboard capability used by Ctrl+V.
type ImageClipboard interface {
	ReadImage(context.Context) ([]byte, error)
}

// Options are the bounded process resources for one interactive SSH session.
type Options struct {
	Request     Request
	Input       io.Reader
	Output      io.Writer
	ErrorOutput io.Writer
	Environment []string
	Clipboard   ImageClipboard
}
