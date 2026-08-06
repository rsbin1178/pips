//nolint:wsl_v5 // Bootstrap framing and bounded stream collectors keep related checks together.
package pluginsupervisor

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/rsbin/pips/internal/jsonx"
)

func readBootstrap(reader io.Reader, maxBytes int) (Bootstrap, error) {
	var lengthBytes [4]byte
	if _, err := io.ReadFull(reader, lengthBytes[:]); err != nil {
		return Bootstrap{}, fmt.Errorf("%w: read frame length", ErrBootstrap)
	}
	length := int(binary.BigEndian.Uint32(lengthBytes[:]))
	if length <= 0 || length > maxBytes {
		return Bootstrap{}, ErrBootstrapTooLarge
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return Bootstrap{}, fmt.Errorf("%w: read frame", ErrBootstrap)
	}
	var bootstrap Bootstrap
	if err := jsonx.Decode(payload, &bootstrap); err != nil {
		return Bootstrap{}, fmt.Errorf("%w: decode frame", ErrBootstrap)
	}
	return bootstrap, nil
}

const stdoutReadErrorGrace = 100 * time.Millisecond

// monitorStdout rejects every byte after the bootstrap frame. stdout is not a
// logging channel and remains open until the plugin exits. A read-close error
// is ambiguous: the child may have just exited, or it may have closed stdout
// while remaining alive. Give cmd.Wait a short opportunity to publish the
// process exit before treating the latter as a protocol violation.
func monitorStdout(
	reader io.Reader,
	violation chan error,
	cancel chan<- struct{},
	done chan<- struct{},
	waitDone <-chan struct{},
) {
	defer close(done)
	defer close(violation)
	buffer := make([]byte, 4096)
	for {
		count, err := reader.Read(buffer)
		if count > 0 {
			select {
			case violation <- ErrBootstrapTrailing:
			default:
			}
			select {
			case cancel <- struct{}{}:
			default:
			}
			return
		}
		if err == nil {
			continue
		}
		if channelClosed(waitDone) {
			return
		}
		timer := time.NewTimer(stdoutReadErrorGrace)
		select {
		case <-waitDone:
			timer.Stop()
			return
		case <-timer.C:
		}
		if channelClosed(waitDone) {
			return
		}
		select {
		case violation <- fmt.Errorf("%w: read stdout", ErrBootstrap):
		default:
		}
		select {
		case cancel <- struct{}{}:
		default:
		}
		return
	}
}

type stderrCollector struct {
	mu             sync.Mutex
	maxBytes       int
	maxLines       int
	maxPerSecond   int
	lines          []string
	bytes          int
	droppedLines   int
	droppedBytes   int
	rateLimited    int
	window         time.Time
	windowLines    int
	partial        []byte
	token          string
	pluginID       string
	artifactDigest string
	pid            int
	records        []StderrRecord
}

func newStderrCollector(c Config, token string) *stderrCollector {
	return &stderrCollector{
		maxBytes:       c.MaxStderrBytes,
		maxLines:       c.MaxStderrLines,
		maxPerSecond:   c.MaxStderrPerSecond,
		token:          token,
		pluginID:       c.PluginID,
		artifactDigest: c.ArtifactDigest,
		window:         time.Now(),
	}
}

func (c *stderrCollector) setProcessIdentity(pid int) {
	c.mu.Lock()
	c.pid = pid
	c.mu.Unlock()
}

func (c *stderrCollector) consume(data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.partial = append(c.partial, data...)
	for {
		index := indexNewline(c.partial)
		if index < 0 {
			if len(c.partial) > c.maxBytes {
				c.partial = c.partial[:c.maxBytes]
			}
			return
		}
		line := append([]byte(nil), c.partial[:index]...)
		c.partial = c.partial[index+1:]
		c.recordLocked(line)
	}
}

func (c *stderrCollector) finish() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.partial) != 0 {
		c.recordLocked(c.partial)
		c.partial = nil
	}
}

func indexNewline(value []byte) int {
	for i, b := range value {
		if b == '\n' {
			return i
		}
	}
	return -1
}

func (c *stderrCollector) recordLocked(line []byte) {
	now := time.Now()
	if now.Sub(c.window) >= time.Second {
		c.window = now
		c.windowLines = 0
	}
	cleaned := sanitizeLine(line)
	if c.token != "" {
		cleaned = strings.ReplaceAll(cleaned, c.token, "[redacted]")
	}
	lineBytes := len(cleaned)
	if c.windowLines >= c.maxPerSecond {
		c.rateLimited++
		c.droppedLines++
		c.droppedBytes += lineBytes
		return
	}
	c.windowLines++
	if len(c.lines) >= c.maxLines || c.bytes+lineBytes > c.maxBytes {
		c.droppedLines++
		c.droppedBytes += lineBytes
		return
	}
	c.lines = append(c.lines, cleaned)
	c.records = append(c.records, StderrRecord{
		PluginID: c.pluginID, ArtifactDigest: c.artifactDigest, PID: c.pid,
		Timestamp: time.Now().UTC(), Message: cleaned,
	})
	c.bytes += lineBytes
}

func sanitizeLine(line []byte) string {
	value := strings.ToValidUTF8(string(line), "\uFFFD")
	result := make([]rune, 0, len(value))
	for _, r := range value {
		if r == '\t' || (r >= 0x20 && r != 0x7f && (r < 0x80 || r > 0x9f)) {
			result = append(result, r)
		}
	}
	return string(result)
}

func (c *stderrCollector) snapshot() StderrSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return StderrSnapshot{
		Lines:        append([]string(nil), c.lines...),
		Records:      append([]StderrRecord(nil), c.records...),
		Bytes:        c.bytes,
		DroppedLines: c.droppedLines, DroppedBytes: c.droppedBytes,
		RateLimited: c.rateLimited,
	}
}

func drainStderr(reader io.Reader, collector *stderrCollector, done chan<- struct{}) {
	defer close(done)
	buffered := bufio.NewReaderSize(reader, 16<<10)
	buffer := make([]byte, 16<<10)
	for {
		count, err := buffered.Read(buffer)
		if count > 0 {
			collector.consume(buffer[:count])
		}
		if err != nil {
			collector.finish()
			return
		}
	}
}
