// Package pluginstore owns Phase 2 installation metadata for pips-native
// executable plugins.
//
// It parses a strict, versioned, non-executing manifest; selects a target;
// verifies artifact bytes by SHA-256; and stores immutable content-addressed
// artifacts plus auditable install and enablement records. It never launches
// a plugin, sends it metadata, implements bootstrap, or publishes Agent tools.
// Plugin state is kept under a separate state root and is never removed when
// an artifact reference is removed. Phase 2 records publisher/source/signature
// declarations as ProvenanceDeclaredUnverified. Signature, publisher,
// revocation, and other supply-chain policy decisions belong to the later
// application policy layer; this package does not treat declared provenance as
// verified or authorized.
//
// ArtifactLease is the ownership boundary for future IntegrationGeneration
// and PluginProcess users: acquire it before publishing a generation that may
// need the artifact, and release it only after that generation/process is
// fully retired. Removing an install record never deletes a leased artifact;
// release performs deferred garbage collection when no record remains. Lease
// markers are store-root files, so another Store instance/process observes the
// pin; a crashed owner leaves a conservative stale marker rather than risking
// early deletion, and stale-marker reclamation is deferred to a later
// ownership-aware maintenance phase. PublicationError reports whether an
// atomic rename or marker removal committed and whether enablement was
// published; callers should inspect the store/current record before retrying
// instead of treating every error as a rolled-back candidate. A non-nil error
// after a committed operation is therefore not evidence that the old lease or
// record still exists.
//
// Install records contain a content-addressed copy of the canonical manifest,
// so Current and later generation construction do not depend on the source
// package directory remaining present. Provenance is recorded as declared but
// unverified metadata and never grants authorization. All mutating and
// read-modify-scan operations use a store-root cross-process lock in addition
// to the in-process mutex. Internal paths reject detectable symlink components;
// Windows junction/reparse-point behavior that cannot be identified portably,
// and same-user replacement races after validation, remain deployment concerns
// for a sandboxed or privileged layer. The store remains non-executing: process
// supervision, trust policy, signatures, and capability authorization belong
// to later application layers. Phase 2 only checks the source artifact's
// executable mode on Unix (at least one execute bit); Windows does not infer
// launchability from POSIX mode bits, so executable-format and launch policy
// checks remain supervisor responsibilities.
package pluginstore
