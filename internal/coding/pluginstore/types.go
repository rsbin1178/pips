//nolint:wsl_v5 // Manifest and record validation intentionally keeps invariants together.
package pluginstore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	manifestSchema = "pips.plugin/v1alpha1"
	recordSchema   = "pips.plugin.install/v1alpha1"
	enableSchema   = "pips.plugin.enablement/v1alpha1"

	// ManifestSchema is the exact non-executing plugin manifest schema.
	ManifestSchema = manifestSchema

	maxManifestBytes       = 256 << 10
	maxRecordBytes         = 1 << 20
	maxArtifactBytes       = 256 << 20
	maxIdentifierBytes     = 128
	maxTextBytes           = 16 << 10
	maxTargetCount         = 64
	maxCapabilityCount     = 64
	maxDecisionRefCount    = 128
	maxStateMetadataBytes  = 256
	maxProvenanceFieldSize = 4 << 10
)

// PluginManifest is non-executing installation metadata. It is deliberately
// separate from the generated agent/plugin/v1 wire messages.
type PluginManifest struct {
	Schema             string              `json:"schema"`
	ID                 string              `json:"id"`
	Version            string              `json:"version"`
	Protocol           ProtocolRange       `json:"protocol"`
	Targets            []TargetArtifact    `json:"targets"`
	CapabilityRequests []string            `json:"capability_requests"`
	State              *StateMetadata      `json:"state,omitempty"`
	Provenance         *ProvenanceMetadata `json:"provenance,omitempty"`
}

// ProtocolRange is the application protocol range declared by an artifact.
// A nil MaxMinor means that the range has no declared upper minor bound.
type ProtocolRange struct {
	Major    uint32  `json:"major"`
	MinMinor uint32  `json:"min_minor"`
	MaxMinor *uint32 `json:"max_minor,omitempty"`
}

// TargetArtifact identifies the immutable executable bytes for one target.
type TargetArtifact struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// TargetPlatform is a target selector independent of the running host.
type TargetPlatform struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

// StateMetadata identifies host-owned state without placing state in the
// content-addressed artifact directory.
type StateMetadata struct {
	Schema  string `json:"schema"`
	Version uint32 `json:"version,omitempty"`
}

// ProvenanceMetadata is publisher-supplied metadata. Signature verification is
// intentionally a later supply-chain phase; this package records declarations
// and never treats them as authorization.
type ProvenanceMetadata struct {
	Publisher string `json:"publisher,omitempty"`
	Source    string `json:"source,omitempty"`
	Signature string `json:"signature,omitempty"`
}

// InstallSource records where an artifact was selected from.
type InstallSource struct {
	Scope     string `json:"scope"`
	Reference string `json:"reference,omitempty"`
}

// Source scopes are recorded in install records and do not grant execution authority.
const (
	SourceUser     = "user"
	SourceProject  = "project"
	SourceLocal    = "local"
	SourceRegistry = "registry"
)

// ProvenanceStatus records what the installer established about declared
// provenance. It does not claim cryptographic verification.
type ProvenanceStatus string

// Provenance statuses distinguish absent metadata from declared metadata that
// has not yet passed a signature/provenance policy.
const (
	ProvenanceAbsent             ProvenanceStatus = "absent"
	ProvenanceDeclaredUnverified ProvenanceStatus = "declared_unverified"
)

// ProvenanceResult is the auditable provenance projection in an install record.
type ProvenanceResult struct {
	Status    ProvenanceStatus `json:"status"`
	Publisher string           `json:"publisher,omitempty"`
	Source    string           `json:"source,omitempty"`
	Signature string           `json:"signature,omitempty"`
}

// InstallRecord is an immutable, auditable record for one installed artifact.
// RequestedVersion and AssociatedBundleDigest are optional install-decision
// bindings; they do not change the resolved artifact Version or make Bundle
// executable.
type InstallRecord struct {
	Schema                 string           `json:"schema"`
	PluginID               string           `json:"plugin_id"`
	Version                string           `json:"version"`
	RequestedVersion       string           `json:"requested_version,omitempty"`
	ManifestDigest         string           `json:"manifest_digest"`
	ManifestPath           string           `json:"manifest_path"`
	ArtifactDigest         string           `json:"artifact_digest"`
	Target                 TargetPlatform   `json:"target"`
	ArtifactPath           string           `json:"artifact_path"`
	StatePath              string           `json:"state_path"`
	Source                 InstallSource    `json:"source"`
	Provenance             ProvenanceResult `json:"provenance"`
	AssociatedBundleDigest string           `json:"associated_bundle_digest,omitempty"`
	CapabilityDecisionRefs []string         `json:"capability_decision_refs"`
	InstalledAt            time.Time        `json:"installed_at"`
}

type enablementRecord struct {
	Schema         string    `json:"schema"`
	PluginID       string    `json:"plugin_id"`
	RecordPath     string    `json:"record_path"`
	ManifestDigest string    `json:"manifest_digest"`
	ArtifactDigest string    `json:"artifact_digest"`
	Version        string    `json:"version"`
	EnabledAt      time.Time `json:"enabled_at"`
}

// Limits bounds manifest, artifact, and durable record sizes.
type Limits struct {
	ManifestBytes int64
	ArtifactBytes int64
	RecordBytes   int64
	Targets       int
	Capabilities  int
	DecisionRefs  int
}

// DefaultLimits returns conservative bounds for installation metadata and
// artifact copying. Artifact bytes are streamed and are never held in memory.
func DefaultLimits() Limits {
	return Limits{
		ManifestBytes: maxManifestBytes,
		ArtifactBytes: maxArtifactBytes,
		RecordBytes:   maxRecordBytes,
		Targets:       maxTargetCount,
		Capabilities:  maxCapabilityCount,
		DecisionRefs:  maxDecisionRefCount,
	}
}

func (l Limits) withDefaults() Limits {
	defaults := DefaultLimits()
	if l.ManifestBytes == 0 {
		l.ManifestBytes = defaults.ManifestBytes
	}
	if l.ArtifactBytes == 0 {
		l.ArtifactBytes = defaults.ArtifactBytes
	}
	if l.RecordBytes == 0 {
		l.RecordBytes = defaults.RecordBytes
	}
	if l.Targets == 0 {
		l.Targets = defaults.Targets
	}
	if l.Capabilities == 0 {
		l.Capabilities = defaults.Capabilities
	}
	if l.DecisionRefs == 0 {
		l.DecisionRefs = defaults.DecisionRefs
	}
	return l
}

func (l Limits) validate() error {
	if l.ManifestBytes < 1 || l.ManifestBytes == int64(^uint64(0)>>1) ||
		l.ArtifactBytes < 1 || l.ArtifactBytes == int64(^uint64(0)>>1) ||
		l.RecordBytes < 1 || l.RecordBytes == int64(^uint64(0)>>1) ||
		l.Targets < 1 || l.Capabilities < 1 || l.DecisionRefs < 1 {
		return fmt.Errorf("%w: invalid store limits", ErrInvalid)
	}
	return nil
}

// InstallOptions controls target selection and the auditable install decision.
// RequestedVersion and AssociatedBundleDigest are recorded as optional
// identity fields; Bundle association is declarative only.
type InstallOptions struct {
	Target                 TargetPlatform
	Source                 InstallSource
	RequestedVersion       string
	AssociatedBundleDigest string
	CapabilityDecisionRefs []string
	// Enable defaults to true. Set it to a non-nil false value to install
	// side-by-side without changing the current enablement.
	Enable *bool
}

// Store owns content-addressed artifacts, install records, enablement
// references, and plugin state roots. It never starts or inspects executable
// plugin code.
type Store struct {
	root   string
	limits Limits
	now    func() time.Time
	mu     sync.Mutex
}

var (
	identifierPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	targetPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	capabilityPattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]*\.v[1-9][0-9]*$`)
)

func validIdentifier(value string) bool {
	return len(value) > 0 && len(value) <= maxIdentifierBytes && utf8.ValidString(value) && identifierPattern.MatchString(value)
}

func validTargetPart(value string) bool {
	return len(value) > 0 && len(value) <= 32 && targetPattern.MatchString(value)
}

func validSupportedTarget(target TargetPlatform) bool {
	if !validTargetPart(target.OS) || !validTargetPart(target.Arch) {
		return false
	}
	switch target.OS {
	case "linux":
		switch target.Arch {
		case "386", "amd64", "arm", "arm64", "loong64", "mips", "mips64", "mips64le", "mipsle", "ppc64", "ppc64le", "riscv64", "s390x":
			return true
		}
	case "darwin":
		return target.Arch == "amd64" || target.Arch == "arm64"
	case "windows":
		return target.Arch == "386" || target.Arch == "amd64" || target.Arch == "arm64"
	}
	return false
}

func validCapability(value string) bool {
	return len(value) > 0 && len(value) <= maxIdentifierBytes && utf8.ValidString(value) && capabilityPattern.MatchString(value)
}

func validText(value string, limit int, allowEmpty bool) bool {
	if value == "" {
		return allowEmpty
	}
	if len(value) > limit || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r == 0 || (r < 0x20 && r != '\n' && r != '\r' && r != '\t') {
			return false
		}
	}
	return true
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

//nolint:gocyclo // Path safety checks are one fail-closed lexical boundary.
func validRelativeArtifactPath(value string) bool {
	if value == "" || !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') || strings.ContainsRune(value, '\\') || strings.ContainsRune(value, ':') {
		return false
	}
	if strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") || path.Clean(value) != value {
		return false
	}
	for part := range strings.SplitSeq(value, "/") {
		if part == "" || part == "." || part == ".." || strings.TrimRight(part, " .") != part || !validPathComponent(part) {
			return false
		}
	}
	return true
}

func validateTarget(target TargetArtifact) error {
	if !validSupportedTarget(TargetPlatform{OS: target.OS, Arch: target.Arch}) {
		return fmt.Errorf("%w: unsupported target %s/%s", ErrUnsupportedTarget, target.OS, target.Arch)
	}
	if !validRelativeArtifactPath(target.Path) {
		return fmt.Errorf("%w: target path", ErrInvalid)
	}
	if !validDigest(target.SHA256) {
		return fmt.Errorf("%w: target sha256", ErrInvalid)
	}
	return nil
}

func validateTargetPlatform(target TargetPlatform) error {
	if !validSupportedTarget(target) {
		return fmt.Errorf("%w: unsupported target %s/%s", ErrUnsupportedTarget, target.OS, target.Arch)
	}
	return nil
}

// Validate checks the manifest against the default installation limits.
func (m PluginManifest) Validate() error {
	return m.validate(DefaultLimits())
}

//nolint:gocyclo // Manifest validation owns all cross-field invariants.
func (m PluginManifest) validate(limits Limits) error {
	if m.Schema != manifestSchema {
		if strings.TrimSpace(m.Schema) == "" {
			return fmt.Errorf("%w: missing manifest schema", ErrInvalid)
		}
		return fmt.Errorf("%w: %s", ErrUnsupportedSchema, m.Schema)
	}
	if !validIdentifier(m.ID) {
		return fmt.Errorf("%w: plugin id", ErrInvalid)
	}
	if !validSemanticVersion(m.Version) {
		return fmt.Errorf("%w: plugin version", ErrInvalid)
	}
	if m.Protocol.Major == 0 || m.Protocol.MaxMinor != nil && *m.Protocol.MaxMinor < m.Protocol.MinMinor {
		return fmt.Errorf("%w: protocol range", ErrInvalid)
	}
	if len(m.Targets) == 0 || len(m.Targets) > limits.Targets {
		return fmt.Errorf("%w: target count", ErrLimitExceeded)
	}
	seenTargets := make(map[string]struct{}, len(m.Targets))
	for _, target := range m.Targets {
		if err := validateTarget(target); err != nil {
			return err
		}
		key := target.OS + "\x00" + target.Arch
		if _, exists := seenTargets[key]; exists {
			return fmt.Errorf("%w: target %s/%s", ErrDuplicate, target.OS, target.Arch)
		}
		seenTargets[key] = struct{}{}
	}
	if len(m.CapabilityRequests) > limits.Capabilities {
		return fmt.Errorf("%w: capability count", ErrLimitExceeded)
	}
	seenCapabilities := make(map[string]struct{}, len(m.CapabilityRequests))
	for _, capability := range m.CapabilityRequests {
		if !validCapability(capability) {
			return fmt.Errorf("%w: capability request", ErrInvalid)
		}
		if _, exists := seenCapabilities[capability]; exists {
			return fmt.Errorf("%w: capability %q", ErrDuplicate, capability)
		}
		seenCapabilities[capability] = struct{}{}
	}
	if m.State != nil {
		if !validSingleLineText(m.State.Schema, maxStateMetadataBytes, false) {
			return fmt.Errorf("%w: state metadata", ErrInvalid)
		}
	}
	if m.Provenance != nil {
		if m.Provenance.Publisher == "" && m.Provenance.Source == "" && m.Provenance.Signature == "" {
			return fmt.Errorf("%w: empty provenance metadata", ErrInvalid)
		}
		if !validSingleLineText(m.Provenance.Publisher, maxProvenanceFieldSize, true) ||
			!validSingleLineText(m.Provenance.Source, maxProvenanceFieldSize, true) ||
			!validSingleLineText(m.Provenance.Signature, maxProvenanceFieldSize, true) {
			return fmt.Errorf("%w: provenance metadata", ErrInvalid)
		}
		if m.Provenance.Signature != "" && m.Provenance.Publisher == "" {
			return fmt.Errorf("%w: provenance signature requires publisher", ErrInvalid)
		}
	}
	return nil
}

//nolint:gocyclo // Semantic-version validation is deliberately explicit and strict.
func validSemanticVersion(value string) bool {
	if len(value) == 0 || len(value) > maxIdentifierBytes || !utf8.ValidString(value) {
		return false
	}
	core := value
	if plus := strings.IndexByte(core, '+'); plus >= 0 {
		if !validSemverIdentifiers(core[plus+1:]) {
			return false
		}
		core = core[:plus]
	}
	if dash := strings.IndexByte(core, '-'); dash >= 0 {
		if !validSemverIdentifiers(core[dash+1:], true) {
			return false
		}
		core = core[:dash]
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" || len(part) > 1 && part[0] == '0' {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

//nolint:gocyclo // SemVer identifier validation keeps numeric and textual rules explicit.
func validSemverIdentifiers(value string, prerelease ...bool) bool {
	if value == "" {
		return false
	}
	for identifier := range strings.SplitSeq(value, ".") {
		if identifier == "" {
			return false
		}
		numeric := true
		for _, r := range identifier {
			if r < '0' || r > '9' {
				numeric = false
				if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && r != '-' {
					return false
				}
			}
		}
		if numeric && len(identifier) > 1 && identifier[0] == '0' && len(prerelease) > 0 && prerelease[0] {
			return false
		}
	}
	return true
}

func canonicalManifestPath(digest string) string {
	return "manifests/sha256/" + digest + ".json"
}

func canonicalArtifactPath(digest string) string {
	return "artifacts/sha256/" + digest
}

func canonicalRecordPath(pluginID, manifestDigest, artifactDigest string) string {
	return "records/" + pluginID + "/" + manifestDigest + "-" + artifactDigest + ".json"
}

func validateSource(source InstallSource) error {
	switch source.Scope {
	case SourceUser, SourceProject, SourceLocal, SourceRegistry:
	default:
		return fmt.Errorf("%w: install source scope", ErrInvalid)
	}
	if !validSingleLineText(source.Reference, maxProvenanceFieldSize, true) {
		return fmt.Errorf("%w: install source reference", ErrInvalid)
	}
	return nil
}

func validSingleLineText(value string, limit int, allowEmpty bool) bool {
	if !validText(value, limit, allowEmpty) {
		return false
	}
	for _, r := range value {
		if isASCIIControl(r) {
			return false
		}
	}
	return true
}

func isASCIIControl(r rune) bool {
	return r <= 0x1f || r >= 0x7f && r <= 0x9f
}

func validPathComponent(value string) bool {
	if !validText(value, 255, false) {
		return false
	}
	for _, r := range value {
		if isASCIIControl(r) {
			return false
		}
	}
	return true
}

func validateDecisionRefs(refs []string, limit int) ([]string, error) {
	if len(refs) > limit {
		return nil, fmt.Errorf("%w: capability decision count", ErrLimitExceeded)
	}
	result := append([]string(nil), refs...)
	for _, ref := range result {
		if !validSingleLineText(ref, maxIdentifierBytes, false) {
			return nil, fmt.Errorf("%w: capability decision reference", ErrInvalid)
		}
	}
	sort.Strings(result)
	for index := 1; index < len(result); index++ {
		if result[index] == result[index-1] {
			return nil, fmt.Errorf("%w: capability decision reference", ErrDuplicate)
		}
	}
	return result, nil
}

//nolint:gocyclo // Record validation binds every persisted identity field.
func validateInstallRecord(record InstallRecord, limits Limits) error {
	if record.Schema != recordSchema || !validIdentifier(record.PluginID) || !validSemanticVersion(record.Version) ||
		!validDigest(record.ManifestDigest) || !validDigest(record.ArtifactDigest) || record.InstalledAt.IsZero() {
		return fmt.Errorf("%w: install record", ErrInvalid)
	}
	if err := validateInstallMetadata(record.RequestedVersion, record.AssociatedBundleDigest); err != nil {
		return err
	}
	if err := validateTargetPlatform(record.Target); err != nil {
		return err
	}
	expectedManifestPath := canonicalManifestPath(record.ManifestDigest)
	expectedArtifactPath := canonicalArtifactPath(record.ArtifactDigest)
	if record.ManifestPath != expectedManifestPath || record.ArtifactPath != expectedArtifactPath || record.StatePath != "state/"+record.PluginID {
		return fmt.Errorf("%w: install record paths", ErrInvalid)
	}
	if err := validateSource(record.Source); err != nil {
		return err
	}
	if record.Provenance.Status != ProvenanceAbsent && record.Provenance.Status != ProvenanceDeclaredUnverified {
		return fmt.Errorf("%w: provenance status", ErrInvalid)
	}
	if record.Provenance.Status == ProvenanceAbsent &&
		(record.Provenance.Publisher != "" || record.Provenance.Source != "" || record.Provenance.Signature != "") {
		return fmt.Errorf("%w: absent provenance has metadata", ErrInvalid)
	}
	if !validSingleLineText(record.Provenance.Publisher, maxProvenanceFieldSize, true) ||
		!validSingleLineText(record.Provenance.Source, maxProvenanceFieldSize, true) ||
		!validSingleLineText(record.Provenance.Signature, maxProvenanceFieldSize, true) {
		return fmt.Errorf("%w: install record provenance", ErrInvalid)
	}
	refs, err := validateDecisionRefs(record.CapabilityDecisionRefs, limits.DecisionRefs)
	if err != nil {
		return err
	}
	if len(refs) != len(record.CapabilityDecisionRefs) {
		return fmt.Errorf("%w: capability decision references", ErrInvalid)
	}
	for index := range refs {
		if refs[index] != record.CapabilityDecisionRefs[index] {
			return fmt.Errorf("%w: capability decision references are not sorted", ErrInvalid)
		}
	}
	return nil
}

func validateInstallMetadata(requestedVersion, associatedBundleDigest string) error {
	if requestedVersion != "" && !validSemanticVersion(requestedVersion) {
		return fmt.Errorf("%w: requested version", ErrInvalid)
	}
	if associatedBundleDigest != "" && !validDigest(associatedBundleDigest) {
		return fmt.Errorf("%w: associated bundle digest", ErrInvalid)
	}
	return nil
}

func validateEnablement(value enablementRecord) error {
	if value.Schema != enableSchema || !validIdentifier(value.PluginID) || value.RecordPath == "" ||
		!validDigest(value.ManifestDigest) || !validDigest(value.ArtifactDigest) || !validSemanticVersion(value.Version) || value.EnabledAt.IsZero() {
		return fmt.Errorf("%w: enablement record", ErrInvalid)
	}
	if value.RecordPath != canonicalRecordPath(value.PluginID, value.ManifestDigest, value.ArtifactDigest) {
		return fmt.Errorf("%w: enablement record path", ErrInvalid)
	}
	return nil
}

func digestBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func cloneManifest(value PluginManifest) PluginManifest {
	value.Targets = append([]TargetArtifact(nil), value.Targets...)
	value.CapabilityRequests = append([]string(nil), value.CapabilityRequests...)
	if value.State != nil {
		state := *value.State
		value.State = &state
	}
	if value.Provenance != nil {
		provenance := *value.Provenance
		value.Provenance = &provenance
	}
	return value
}

func fileModeForArtifact(mode fs.FileMode) fs.FileMode {
	return mode.Perm()
}
