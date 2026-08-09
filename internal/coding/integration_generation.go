//nolint:wsl_v5 // Generation ownership keeps lock and lifecycle transitions adjacent.
package coding

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/rsbin/pips/agent/extension"
	"github.com/rsbin/pips/internal/coding/agentplugin"
	"github.com/rsbin/pips/internal/coding/agentprofile"
	"github.com/rsbin/pips/internal/coding/hooks"
	codingmcp "github.com/rsbin/pips/internal/coding/mcp"
	"github.com/rsbin/pips/internal/coding/resource"
	"github.com/rsbin/pips/internal/coding/skillsettings"
)

var errIntegrationGenerationRetired = errors.New("coding integration generation: retired")

// IntegrationGeneration is one immutable, application-owned snapshot of the
// interaction-visible integrations. Its owner is the Runtime; interactions
// acquire leases and keep the complete snapshot until they settle.
//
// The Extension Activation and MCP Connections are leases over executable
// state. They are released together with the generation, after all interaction
// leases have drained.
type IntegrationGeneration struct {
	id                  uint64
	activation          *extension.Activation
	connections         *codingmcp.Connections
	resources           resource.Result
	agentProfiles       agentprofile.Registry
	skillPolicy         skillsettings.Snapshot
	projectInstructions string
	agentPlugins        agentplugin.Result
	hookDefinitions     []hooks.Definition

	mu        sync.Mutex
	refs      int // Runtime ownership plus interaction leases.
	retired   bool
	handedOff bool
	closeOnce sync.Once
	closeErr  error
}

func newIntegrationGeneration(
	id uint64,
	activation *extension.Activation,
	connections *codingmcp.Connections,
	resources resource.Result,
	agentProfiles agentprofile.Registry,
	skillPolicy skillsettings.Snapshot,
	projectInstructions string,
	agentPlugins agentplugin.Result,
	hookDefinitions []hooks.Definition,
) *IntegrationGeneration {
	return &IntegrationGeneration{
		id:                  id,
		activation:          activation,
		connections:         connections,
		resources:           resources,
		agentProfiles:       agentProfiles.Clone(),
		skillPolicy:         skillPolicy.Clone(),
		projectInstructions: projectInstructions,
		agentPlugins:        agentPlugins,
		hookDefinitions:     slices.Clone(hookDefinitions),
		refs:                1,
	}
}

// ID identifies the immutable generation for diagnostics and tests.
func (g *IntegrationGeneration) ID() uint64 {
	if g == nil {
		return 0
	}

	return g.id
}

func (g *IntegrationGeneration) acquire() error {
	if g == nil {
		return errIntegrationGenerationRetired
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.retired || g.refs == 0 {
		return errIntegrationGenerationRetired
	}

	g.refs++

	return nil
}

func (g *IntegrationGeneration) release(ctx context.Context) error {
	if g == nil {
		return nil
	}

	g.mu.Lock()
	if g.refs == 0 {
		g.mu.Unlock()
		return nil
	}
	g.refs--
	closeNow := g.retired && g.refs == 0 && !g.handedOff
	g.mu.Unlock()
	if !closeNow {
		return nil
	}

	return g.close(ctx)
}

// retire releases the Runtime's ownership. The generation remains alive until
// every interaction lease is released.
func (g *IntegrationGeneration) retire(ctx context.Context) error {
	if g == nil {
		return nil
	}

	g.mu.Lock()
	if g.retired {
		closed := g.refs == 0
		g.mu.Unlock()
		if closed {
			return g.close(ctx)
		}

		return nil
	}
	g.retired = true
	g.mu.Unlock()

	return g.release(ctx)
}

// handoff transfers the current generation's owned resources to a new
// immutable generation. It is only valid at the idle boundary, where the
// Runtime owns the sole generation reference. This is used for policy-only
// publication such as a Skill enablement change without reconnecting MCP or
// reactivating Extensions.
func (g *IntegrationGeneration) handoff(
	id uint64,
	skillPolicy skillsettings.Snapshot,
) (*IntegrationGeneration, error) {
	if g == nil {
		return nil, errIntegrationGenerationRetired
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.retired || g.handedOff || g.refs != 1 {
		return nil, errIntegrationGenerationRetired
	}

	g.handedOff = true
	candidate := &IntegrationGeneration{
		id:                  id,
		activation:          g.activation,
		connections:         g.connections,
		resources:           g.resources,
		agentProfiles:       g.agentProfiles.Clone(),
		skillPolicy:         skillPolicy.Clone(),
		projectInstructions: g.projectInstructions,
		agentPlugins:        g.agentPlugins,
		hookDefinitions:     slices.Clone(g.hookDefinitions),
		refs:                1,
	}
	g.activation = nil
	g.connections = nil
	g.resources = resource.Result{}
	g.agentProfiles = agentprofile.Registry{}
	g.agentPlugins = agentplugin.Result{}
	g.hookDefinitions = nil
	g.refs = 0
	g.retired = true

	return candidate, nil
}

func (g *IntegrationGeneration) close(ctx context.Context) error {
	if g == nil {
		return nil
	}

	g.closeOnce.Do(func() {
		errs := make([]error, 0, 2)
		if g.activation != nil {
			if err := g.activation.Release(ctx); err != nil {
				errs = append(errs, err)
			}
		}
		if g.connections != nil {
			if err := g.connections.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		g.mu.Lock()
		g.closeErr = errors.Join(errs...)
		g.mu.Unlock()
	})

	g.mu.Lock()
	err := g.closeErr
	g.mu.Unlock()

	return err
}

func (g *IntegrationGeneration) snapshot() *extension.Activation {
	if g == nil {
		return nil
	}

	return g.activation
}

func (g *IntegrationGeneration) connectionsSnapshot() *codingmcp.Connections {
	if g == nil {
		return nil
	}

	return g.connections
}

func (g *IntegrationGeneration) resourcesSnapshot() resource.Result {
	if g == nil {
		return resource.Result{}
	}

	return g.resources
}

func (g *IntegrationGeneration) agentProfilesSnapshot() agentprofile.Registry {
	if g == nil {
		return agentprofile.Registry{}
	}

	return g.agentProfiles.Clone()
}

func (g *IntegrationGeneration) skillPolicySnapshot() skillsettings.Snapshot {
	if g == nil {
		return skillsettings.Empty()
	}

	return g.skillPolicy.Clone()
}

func (g *IntegrationGeneration) projectInstructionsSnapshot() string {
	if g == nil {
		return ""
	}

	return g.projectInstructions
}

func (g *IntegrationGeneration) agentPluginSnapshot() agentplugin.Result {
	if g == nil {
		return agentplugin.Result{}
	}

	return g.agentPlugins
}

func (g *IntegrationGeneration) hookDefinitionsSnapshot() []hooks.Definition {
	if g == nil {
		return nil
	}

	return slices.Clone(g.hookDefinitions)
}

func (g *IntegrationGeneration) skillEntries(base []extension.SkillEntry) []extension.SkillEntry {
	if g == nil {
		return slices.Clone(base)
	}

	return slices.Concat(base, g.agentPlugins.SkillEntries())
}

func (r *Runtime) nextGenerationID() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.generationID + 1
}
