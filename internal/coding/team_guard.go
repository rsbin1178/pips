package coding

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/catalog"
	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/internal/coding/subagent"
	"github.com/rsbin/pips/internal/coding/tools"
)

type teamGuardState uint8

const (
	teamGuardInactive teamGuardState = iota
	teamGuardProposed
	teamGuardActive
	teamGuardClosing
)

// teamCapabilityGuard is interaction-independent. Hooks leased by an already
// running Lead interaction consult it at call time, so Team activation closes
// the write boundary without mutating the interaction's immutable catalog.
type teamCapabilityGuard struct {
	mu     sync.RWMutex
	state  teamGuardState
	teamID team.ID
}

func (g *teamCapabilityGuard) propose() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.state != teamGuardInactive {
		return fmt.Errorf("%w: Team admission is already active", ErrTeamActive)
	}

	g.state = teamGuardProposed

	return nil
}

func (g *teamCapabilityGuard) decline() {
	g.mu.Lock()
	if g.state == teamGuardProposed {
		g.state = teamGuardInactive
	}
	g.mu.Unlock()
}

func (g *teamCapabilityGuard) activate(id team.ID) error {
	if id == "" {
		return fmt.Errorf("%w: empty Team identity", ErrTeamAdmission)
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if g.state != teamGuardProposed &&
		!(g.state == teamGuardActive && g.teamID == id) {
		return fmt.Errorf("%w: Team guard is not awaiting confirmation", ErrTeamAdmission)
	}

	g.state = teamGuardActive
	g.teamID = id

	return nil
}

func (g *teamCapabilityGuard) resume(id team.ID) error {
	if id == "" {
		return fmt.Errorf("%w: empty Team identity", ErrTeamAdmission)
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state != teamGuardInactive {
		return ErrTeamActive
	}

	g.state = teamGuardActive
	g.teamID = id

	return nil
}

func (g *teamCapabilityGuard) beginClose(id team.ID) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.state != teamGuardActive || g.teamID != id {
		return false
	}

	g.state = teamGuardClosing

	return true
}

func (g *teamCapabilityGuard) deactivate(id team.ID) {
	g.mu.Lock()
	if g.teamID == id && (g.state == teamGuardActive || g.state == teamGuardClosing) {
		g.state = teamGuardInactive
		g.teamID = ""
	}
	g.mu.Unlock()
}

func (g *teamCapabilityGuard) active() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()

	return g.state == teamGuardActive || g.state == teamGuardClosing
}

func (g *teamCapabilityGuard) admissionOpen() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()

	return g.state == teamGuardProposed
}

func (g *teamCapabilityGuard) snapshot() (teamGuardState, team.ID) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	return g.state, g.teamID
}

func (g *teamCapabilityGuard) beforeTool(
	descriptors []catalog.Descriptor,
) func(context.Context, agent.ToolCallInfo) agent.ToolDecision {
	byName := make(map[string]catalog.Descriptor, len(descriptors))
	for _, descriptor := range descriptors {
		byName[descriptor.Name] = descriptor
	}

	return func(_ context.Context, info agent.ToolCallInfo) agent.ToolDecision {
		if !g.active() {
			return agent.ToolDecision{}
		}

		descriptor, found := byName[info.Name]
		if !found {
			return agent.DenyTool(fmt.Sprintf(
				"tool %q is unavailable while the Lead coordinates an active Team",
				info.Name,
			))
		}

		if leadTeamDescriptorAllowed(descriptor) {
			return agent.ToolDecision{}
		}

		return agent.DenyTool(fmt.Sprintf(
			"tool %q is unavailable while the Lead coordinates an active Team",
			info.Name,
		))
	}
}

func leadTeamDescriptorAllowed(descriptor catalog.Descriptor) bool {
	if descriptor.Source.Kind == catalog.SourceTeam {
		return true
	}
	if descriptor.Source.Kind != catalog.SourceLocal {
		return false
	}
	if descriptor.Name == subagent.ToolName || descriptor.Name == subagent.SpawnToolName ||
		descriptor.Source.ID == tools.PlanCatalogID {
		return false
	}

	return descriptor.Risk == catalog.RiskRead
}

func filterCatalog(
	ctx context.Context,
	value *catalog.Catalog,
	keep func(catalog.Descriptor) bool,
) (*catalog.Catalog, error) {
	if value == nil || keep == nil {
		return nil, fmt.Errorf("%w: invalid catalog filter", ErrRuntimeInvalid)
	}

	policy := catalog.AllowAll("coding-team-filter", catalog.RiskPrivileged)
	descriptors, err := value.Search(ctx, policy, "")
	if err != nil {
		return nil, err
	}

	entries := make([]catalog.Entry, 0, len(descriptors))
	for _, descriptor := range descriptors {
		if !keep(descriptor) {
			continue
		}

		selected, selectErr := value.Tools(ctx, policy, descriptor.Name)
		if selectErr != nil {
			return nil, selectErr
		}
		if len(selected) != 1 {
			return nil, fmt.Errorf("%w: catalog filter lost tool %q", ErrRuntimeInvalid, descriptor.Name)
		}

		entries = append(entries, catalog.Entry{
			Tool: selected[0], Source: descriptor.Source, Risk: descriptor.Risk,
			Tags: slices.Clone(descriptor.Tags),
		})
	}

	return catalog.New(entries...)
}
