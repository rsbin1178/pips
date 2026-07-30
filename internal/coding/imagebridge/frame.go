package imagebridge

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/rsbin/pips/internal/coding/attachment"
)

const (
	frameVersion     = 1
	frameHeaderBytes = 56
	maximumFrameAge  = 30 * time.Second

	frameTypeImage  = 1
	frameTypeNotice = 2

	mediaPNG  = 1
	mediaJPEG = 2
)

var frameMagic = [8]byte{'P', 'I', 'P', 'S', 'I', 'B', '0', '1'}

// Notice is an allowlisted bridge acknowledgement.
type Notice uint8

const (
	// NoticeUnknown is the zero value and never valid on the wire.
	NoticeUnknown Notice = iota
	// NoticeAccepted confirms that the live TUI accepted an image.
	NoticeAccepted
	// NoticeBusy reports that the fixed live TUI inbox is full.
	NoticeBusy
	// NoticeRejected reports invalid image input without echoing details.
	NoticeRejected
)

// Frame is one validated image or notice with its original absolute deadline.
type Frame struct {
	Image    attachment.Image
	Notice   Notice
	Deadline time.Time
}

// Encode writes one complete strict frame. The caller owns framing EOF, such
// as closing the write half of a Unix connection after this function returns.
func Encode(writer io.Writer, frame Frame) error {
	if writer == nil {
		return fmt.Errorf("%w: missing writer", ErrProtocol)
	}

	header := make([]byte, frameHeaderBytes)
	copy(header[:8], frameMagic[:])
	header[8] = frameVersion

	var body []byte

	switch {
	case frame.Image.Valid() && frame.Notice == NoticeUnknown:
		header[9] = frameTypeImage

		media, err := encodedMedia(frame.Image.MIMEType())
		if err != nil {
			return err
		}

		header[11] = media
		body = frame.Image.Bytes()
	case !frame.Image.Valid() && validNotice(frame.Notice):
		header[9] = frameTypeNotice
		header[10] = byte(frame.Notice)
	default:
		return fmt.Errorf("%w: frame must contain exactly one payload", ErrProtocol)
	}

	deadlineMillis := frame.Deadline.UnixMilli()

	bodyLength := len(body)

	if frame.Deadline.IsZero() || deadlineMillis <= 0 {
		return ErrDeadline
	}

	if bodyLength > attachment.MaxImageBytes || uint64(bodyLength) > math.MaxUint32 {
		return fmt.Errorf("%w: frame body exceeds limit", ErrProtocol)
	}

	binary.BigEndian.PutUint32(header[12:16], uint32(bodyLength))
	binary.BigEndian.PutUint64(header[16:24], uint64(deadlineMillis))

	digest := sha256.Sum256(body)
	copy(header[24:], digest[:])

	if err := writeFull(writer, header); err != nil {
		return fmt.Errorf("%w: write frame header", ErrProtocol)
	}

	if err := writeFull(writer, body); err != nil {
		return fmt.Errorf("%w: write frame body", ErrProtocol)
	}

	return nil
}

// Decode reads exactly one strict frame and requires EOF after its body.
func Decode(ctx context.Context, reader io.Reader, now time.Time) (Frame, error) {
	if err := ctx.Err(); err != nil {
		return Frame{}, err
	}

	if reader == nil || now.IsZero() {
		return Frame{}, fmt.Errorf("%w: missing reader or clock", ErrProtocol)
	}

	header, body, err := readValidatedFrame(reader, now)
	if err != nil {
		return Frame{}, err
	}

	if header.kind == frameTypeNotice {
		return Frame{Notice: header.notice, Deadline: header.deadline}, nil
	}

	image, err := attachment.NormalizeImageContext(ctx, "clipboard.png", body)
	if err != nil {
		return Frame{}, fmt.Errorf("%w: normalize image", errors.Join(ErrProtocol, err))
	}

	if image.MIMEType() != header.mimeType || image.Size() != len(body) || image.Digest() != header.digest {
		return Frame{}, fmt.Errorf("%w: image metadata changed", ErrProtocol)
	}

	return Frame{Image: image, Deadline: header.deadline}, nil
}

func readValidatedFrame(reader io.Reader, now time.Time) (decodedFrameHeader, []byte, error) {
	headerBytes := make([]byte, frameHeaderBytes)
	if _, err := io.ReadFull(reader, headerBytes); err != nil {
		return decodedFrameHeader{}, nil, fmt.Errorf("%w: truncated header", ErrProtocol)
	}

	header, err := decodeFrameHeader(headerBytes, now)
	if err != nil {
		return decodedFrameHeader{}, nil, err
	}

	body := make([]byte, header.length)
	if _, err := io.ReadFull(reader, body); err != nil {
		return decodedFrameHeader{}, nil, fmt.Errorf("%w: truncated body", ErrProtocol)
	}

	if err := requireFrameEOF(reader); err != nil {
		return decodedFrameHeader{}, nil, err
	}

	digest := sha256.Sum256(body)
	if digest != header.digest {
		return decodedFrameHeader{}, nil, fmt.Errorf("%w: digest mismatch", ErrProtocol)
	}

	return header, body, nil
}

type decodedFrameHeader struct {
	kind     byte
	notice   Notice
	mimeType string
	length   int
	deadline time.Time
	digest   [sha256.Size]byte
}

func decodeFrameHeader(value []byte, now time.Time) (decodedFrameHeader, error) {
	if len(value) != frameHeaderBytes || string(value[:8]) != string(frameMagic[:]) ||
		value[8] != frameVersion || value[9] != frameTypeImage && value[9] != frameTypeNotice {
		return decodedFrameHeader{}, ErrProtocol
	}

	deadline, err := decodeFrameDeadline(value[16:24], now)
	if err != nil {
		return decodedFrameHeader{}, err
	}

	header := decodedFrameHeader{
		kind: value[9], length: int(binary.BigEndian.Uint32(value[12:16])), deadline: deadline,
	}
	copy(header.digest[:], value[24:])

	if header.kind == frameTypeNotice {
		return decodeNoticeHeader(value, header)
	}

	return decodeImageHeader(value, header)
}

func decodeFrameDeadline(value []byte, now time.Time) (time.Time, error) {
	deadlineMillis := binary.BigEndian.Uint64(value)
	if deadlineMillis > math.MaxInt64 {
		return time.Time{}, ErrDeadline
	}

	deadline := time.UnixMilli(int64(deadlineMillis))
	if !deadline.After(now) || deadline.After(now.Add(maximumFrameAge)) {
		return time.Time{}, ErrDeadline
	}

	return deadline, nil
}

func decodeNoticeHeader(value []byte, header decodedFrameHeader) (decodedFrameHeader, error) {
	header.notice = Notice(value[10])
	if value[11] != 0 || header.length != 0 || !validNotice(header.notice) {
		return decodedFrameHeader{}, fmt.Errorf("%w: invalid notice", ErrProtocol)
	}

	return header, nil
}

func decodeImageHeader(value []byte, header decodedFrameHeader) (decodedFrameHeader, error) {
	if value[10] != 0 || header.length == 0 || header.length > attachment.MaxImageBytes {
		return decodedFrameHeader{}, fmt.Errorf("%w: invalid image metadata", ErrProtocol)
	}

	mimeType, err := decodedMedia(value[11])
	if err != nil {
		return decodedFrameHeader{}, err
	}

	header.mimeType = mimeType

	return header, nil
}

func validNotice(notice Notice) bool {
	return notice == NoticeAccepted || notice == NoticeBusy || notice == NoticeRejected
}

func encodedMedia(mimeType string) (byte, error) {
	switch mimeType {
	case "image/png":
		return mediaPNG, nil
	case "image/jpeg":
		return mediaJPEG, nil
	default:
		return 0, fmt.Errorf("%w: unsupported normalized media", ErrProtocol)
	}
}

func decodedMedia(value byte) (string, error) {
	switch value {
	case mediaPNG:
		return "image/png", nil
	case mediaJPEG:
		return "image/jpeg", nil
	default:
		return "", fmt.Errorf("%w: unsupported media code", ErrProtocol)
	}
}

func requireFrameEOF(reader io.Reader) error {
	var trailing [1]byte

	count, err := reader.Read(trailing[:])
	if count != 0 || err == nil {
		return fmt.Errorf("%w: trailing data", ErrProtocol)
	}

	if !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: read trailer", ErrProtocol)
	}

	return nil
}

func writeFull(writer io.Writer, value []byte) error {
	for len(value) > 0 {
		count, err := writer.Write(value)
		if count > 0 {
			value = value[count:]
		}

		if err != nil {
			return err
		}

		if count == 0 {
			return io.ErrShortWrite
		}
	}

	return nil
}
