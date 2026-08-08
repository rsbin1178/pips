//nolint:wsl_v5 // Candidate acquisition order mirrors reverse-order cleanup ownership.
package coding

import (
	"context"
	"errors"
	"fmt"

	"github.com/rsbin/pips/internal/coding/agentplugin"
	"github.com/rsbin/pips/internal/coding/resource"
)

// integrationCandidate owns a complete, unpublished Coding integration
// snapshot. It is intentionally specific to Runtime reload: abort releases
// acquired resources in reverse order, while publish transfers them to the
// immutable IntegrationGeneration.
type integrationCandidate struct {
	generation    *IntegrationGeneration
	activationErr error
	cleanup       cleanupStack
}

// buildIntegrationCandidate acquires every reload-visible integration input
// off to the side. Nothing becomes interaction-visible until Reload publishes
// the returned generation at its idle safe boundary.
func (r *Runtime) buildIntegrationCandidate(
	ctx context.Context,
) (_ *integrationCandidate, returnErr error) {
	candidate := &integrationCandidate{}
	defer func() {
		if returnErr == nil {
			return
		}

		returnErr = errors.Join(returnErr, candidate.abort(context.WithoutCancel(ctx)))
	}()

	plugins, err := agentplugin.Load(ctx, agentplugin.Options{
		Paths: r.paths, Tree: r.tree, ProjectTrusted: r.trusted, Limits: r.opts.AgentPluginLimits,
	})
	if err != nil {
		return nil, err
	}
	loaded, err := resource.Load(ctx, resource.Options{
		Paths: r.paths, Tree: r.tree, ProjectTrusted: r.trusted, Limits: r.opts.ResourceLimits,
	})
	if err != nil {
		return nil, err
	}
	nextSkillPolicy, err := r.skillSettings.Load(ctx)
	if err != nil {
		return nil, err
	}
	nextProjectInstructions, err := r.instructionResolver.Resolve(ctx, ".")
	if err != nil {
		return nil, fmt.Errorf("coding runtime: resolve project instructions: %w", err)
	}

	options := OpenOptions{
		Workspace: r.workspace, Trusted: r.trusted, Paths: r.paths,
	}
	connections, err := openMCP(
		ctx, options, r.opts, r.tree, r.permissions, plugins.MCPDefinitions(),
	)
	if err != nil {
		return nil, err
	}
	candidate.cleanup.add(func(context.Context) error { return connections.Close() })

	activation, activationErr := activateResources(ctx, r.extensions, loaded, r.compiled)
	if activation == nil {
		if activationErr == nil {
			activationErr = fmt.Errorf("%w: extension activation returned no lease", ErrRuntimeInvalid)
		}

		return nil, activationErr
	}
	candidate.cleanup.add(activation.Release)
	candidate.generation = newIntegrationGeneration(
		r.nextGenerationID(),
		activation,
		connections,
		loaded,
		nextSkillPolicy,
		nextProjectInstructions.SystemPrompt(),
		plugins,
	)
	candidate.activationErr = activationErr

	return candidate, nil
}

// publish transfers the complete candidate to the Runtime. It is only called
// while Reload holds the publication lock after every safe-boundary check.
func (c *integrationCandidate) publish() (*IntegrationGeneration, error) {
	if c == nil || c.generation == nil {
		return nil, fmt.Errorf("%w: missing generation", ErrRuntimeInvalid)
	}

	generation := c.generation
	c.generation = nil
	c.cleanup.values = nil

	return generation, nil
}

// abort releases an unpublished candidate. It is safe to call repeatedly and
// becomes a no-op after publish has transferred ownership.
func (c *integrationCandidate) abort(ctx context.Context) error {
	if c == nil {
		return nil
	}

	cleanup := c.cleanup
	c.cleanup.values = nil
	c.generation = nil

	return cleanup.close(ctx)
}
