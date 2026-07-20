// Package bundle loads bounded local Bundles and activates their
// declarative resources through an extension.Runtime.
//
// A Bundle is distribution metadata, not executable Go code. Its manifest may
// select trusted Extensions already registered by the embedding application
// and may contribute Agent Skills, prompt templates, and opaque typed assets.
// Scripts bundled beside a Skill remain files: this package never executes
// them or implicitly turns them into Tools.
package bundle
