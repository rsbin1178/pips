package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

// SearchSchema names the derived search document format.
const SearchSchema = "pips.coding.session.search/v1"

// Search document bounds. A document is a bounded projection of one session so
// a search pass never has to open a transcript.
const (
	// MaxSearchPrompts caps how many user prompts one document retains.
	MaxSearchPrompts = 32
	// MaxSearchPromptBytes caps the length of one retained prompt.
	MaxSearchPromptBytes = 512
	// MaxSearchContentBytes caps the combined size of all retained prompts.
	MaxSearchContentBytes = 8 << 10
)

// maxSearchDocBytes bounds the encoded document read from disk.
const maxSearchDocBytes = 64 << 10

// ErrSearch identifies an invalid or unreadable search document.
var ErrSearch = errors.New("coding session: invalid search document")

// SearchDoc is one session's derived search document. It is never
// authoritative: it can always be rebuilt from the transcript and the session
// metadata record.
type SearchDoc struct {
	Schema      string    `json:"schema"`
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	Title       string    `json:"title,omitempty"`
	Summary     string    `json:"summary,omitempty"`
	Prompts     []string  `json:"prompts,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
	Hash        string    `json:"hash"`
}

// BuildSearchDoc projects a metadata record and its prompts into a search
// document with a normalized, bounded prompt list and a content hash.
func BuildSearchDoc(record MetadataRecord, prompts []string) SearchDoc {
	doc := SearchDoc{
		Schema:      SearchSchema,
		ID:          record.ID,
		WorkspaceID: record.Workspace.ID,
		Title:       record.Title.Text,
		Summary:     record.Summary,
		Prompts:     normalizeSearchPrompts(prompts),
		UpdatedAt:   record.UpdatedAt,
	}

	doc.Hash = hashSearchContent(doc.Content())

	return doc
}

// ReadSearchDoc reads and validates one search document. A missing file is
// reported as an [os.ErrNotExist] so the caller can rebuild the document.
func ReadSearchDoc(path string) (SearchDoc, error) {
	file, err := os.Open(path) //nolint:gosec // caller-owned store path
	if err != nil {
		return SearchDoc{}, err
	}
	defer func() { _ = file.Close() }()

	data, err := readBoundedFile(file, maxSearchDocBytes)
	if err != nil {
		return SearchDoc{}, err
	}

	return decodeSearchDoc(data)
}

// WriteSearchDoc validates and atomically publishes one search document.
func WriteSearchDoc(path string, doc SearchDoc) error {
	if err := validateSearchDoc(doc); err != nil {
		return err
	}

	encoded, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("coding session: encode search document: %w", err)
	}

	return writeFileAtomic(path, append(encoded, '\n'), 0o600)
}

// Match reports whether every whitespace-separated token of query occurs in the
// document content, case-insensitively. An empty query never matches.
func (d SearchDoc) Match(query string) bool {
	tokens := strings.Fields(strings.ToLower(query))
	if len(tokens) == 0 {
		return false
	}

	content := strings.ToLower(d.Content())
	for _, token := range tokens {
		if !strings.Contains(content, token) {
			return false
		}
	}

	return true
}

// Content returns the searchable text of the document: title, summary, and
// prompts joined by newlines with empty parts skipped.
func (d SearchDoc) Content() string {
	parts := make([]string, 0, len(d.Prompts)+2)
	for _, part := range append([]string{d.Title, d.Summary}, d.Prompts...) {
		if part != "" {
			parts = append(parts, part)
		}
	}

	return strings.Join(parts, "\n")
}

func decodeSearchDoc(data []byte) (SearchDoc, error) {
	var doc SearchDoc

	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&doc); err != nil {
		return SearchDoc{}, fmt.Errorf("coding session: decode search document: %w", err)
	}

	if err := validateSearchDoc(doc); err != nil {
		return SearchDoc{}, err
	}

	return doc, nil
}

func validateSearchDoc(doc SearchDoc) error {
	if doc.Schema != SearchSchema {
		return fmt.Errorf("%w: unsupported schema %q", ErrSearch, doc.Schema)
	}

	if strings.TrimSpace(doc.ID) == "" {
		return fmt.Errorf("%w: empty session id", ErrSearch)
	}

	return nil
}

func hashSearchContent(content string) string {
	sum := sha256.Sum256([]byte(content))

	return hex.EncodeToString(sum[:])
}

// normalizeSearchPrompts trims, deduplicates, truncates, and bounds a prompt
// list. It always returns a non-nil slice.
func normalizeSearchPrompts(prompts []string) []string {
	capacity := min(len(prompts), MaxSearchPrompts)

	normalized := make([]string, 0, capacity)
	seen := make(map[string]struct{}, capacity)
	total := 0

	for _, prompt := range prompts {
		if len(normalized) >= MaxSearchPrompts {
			break
		}

		candidate := truncateSearchPrompt(strings.TrimSpace(prompt))
		if candidate == "" {
			continue
		}

		if _, ok := seen[candidate]; ok {
			continue
		}

		if total+len(candidate) > MaxSearchContentBytes {
			break
		}

		seen[candidate] = struct{}{}
		total += len(candidate)
		normalized = append(normalized, candidate)
	}

	return normalized
}

// truncateSearchPrompt cuts a prompt to the per-prompt byte limit without
// splitting a UTF-8 rune.
func truncateSearchPrompt(prompt string) string {
	if len(prompt) <= MaxSearchPromptBytes {
		return prompt
	}

	truncated := prompt[:MaxSearchPromptBytes]
	for len(truncated) > 0 && !utf8.ValidString(truncated) {
		truncated = truncated[:len(truncated)-1]
	}

	return truncated
}
