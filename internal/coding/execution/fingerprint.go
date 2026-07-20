package execution

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"strconv"
	"strings"
	"time"
)

const (
	operationSchema = "pips.coding.operation/v1alpha1"
	sandboxContract = "pips.coding.sandbox/v1alpha1"
)

// Fingerprint is the complete stable identity of one operation permission request.
type Fingerprint [sha256.Size]byte

// Fingerprint returns the operation's canonical permission identity.
func (o Operation) Fingerprint() Fingerprint {
	digest := sha256.New()
	encoder := fingerprintEncoder{hash: digest}
	encoder.text(operationSchema)
	encoder.text(sandboxContract)
	encoder.text(o.workspaceKey)
	encoder.number(uint64(o.kind))
	encoder.text(o.tool)
	encoder.file(o.executable)
	encoder.number(uint64(len(o.args)))

	for _, argument := range o.args {
		encoder.text(argument)
	}

	encoder.text(o.cwd)
	encoder.number(uint64(len(o.env)))

	for _, variable := range o.env {
		encoder.text(variable.Name)
		encoder.text(variable.Value)
	}

	stdinDigest := sha256.Sum256(o.stdin)
	encoder.number(uint64(len(o.stdin)))
	encoder.bytes(stdinDigest[:])
	encoder.duration(o.timeout)
	encoder.signed(o.output.CaptureBytes)
	encoder.signed(o.output.MaxBytes)
	encoder.integer(o.output.ChunkBytes)
	encoder.integer(o.output.QueueDepth)
	encoder.number(uint64(o.workspace))
	encoder.number(uint64(o.network))
	encoder.number(uint64(len(o.writeDirs)))

	for _, directory := range o.writeDirs {
		encoder.file(directory)
	}

	var fingerprint Fingerprint
	copy(fingerprint[:], digest.Sum(nil))

	return fingerprint
}

// String returns the lowercase hexadecimal persistence form.
func (f Fingerprint) String() string { return hex.EncodeToString(f[:]) }

// ParseFingerprint parses an exact persistence form.
func ParseFingerprint(input string) (Fingerprint, error) {
	if len(input) != hex.EncodedLen(sha256.Size) || input != strings.ToLower(input) {
		return Fingerprint{}, fmt.Errorf("%w: expected %d hexadecimal characters", ErrInvalidFingerprint, hex.EncodedLen(sha256.Size))
	}

	decoded, err := hex.DecodeString(input)
	if err != nil {
		return Fingerprint{}, fmt.Errorf("%w: %w", ErrInvalidFingerprint, err)
	}

	var fingerprint Fingerprint
	copy(fingerprint[:], decoded)

	return fingerprint, nil
}

type fingerprintEncoder struct {
	hash hash.Hash
}

func (e fingerprintEncoder) number(value uint64) {
	var buffer [8]byte
	binary.BigEndian.PutUint64(buffer[:], value)
	_, _ = e.hash.Write(buffer[:])
}

func (e fingerprintEncoder) signed(value int64) {
	_ = binary.Write(e.hash, binary.BigEndian, value)
}

func (e fingerprintEncoder) duration(value time.Duration) {
	_ = binary.Write(e.hash, binary.BigEndian, value)
}

func (e fingerprintEncoder) integer(value int) { e.text(strconv.Itoa(value)) }

func (e fingerprintEncoder) text(value string) { e.bytes([]byte(value)) }

func (e fingerprintEncoder) bytes(value []byte) {
	e.number(uint64(len(value)))
	_, _ = e.hash.Write(value)
}

func (e fingerprintEncoder) file(object fileObject) {
	e.text(object.path)
	e.number(object.device)
	e.number(object.inode)
}
