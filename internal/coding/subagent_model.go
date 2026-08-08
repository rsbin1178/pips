//nolint:wsl_v5 // Model selection keeps catalog, credential, and policy checks adjacent.
package coding

import (
	"context"
	"fmt"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/credential"
	"github.com/rsbin/pips/internal/coding/generation"
	"github.com/rsbin/pips/internal/coding/model"
	"github.com/rsbin/pips/internal/coding/modelcatalog"
	"github.com/rsbin/pips/internal/coding/subagent"
)

// childModelResolver is an interaction-scoped, immutable binding between a
// declarative profile model selection and Runtime-owned configured models. A
// profile can select only a catalog entry; it can never provide a provider
// endpoint, credential, or request-policy override.
type childModelResolver struct {
	catalog       modelcatalog.Catalog
	base          modelcatalog.ResolvedModel
	baseModel     ai.LanguageModel
	basePolicy    generation.Policy
	credentials   credential.Store
	decorateModel func(ai.LanguageModel) ai.LanguageModel
}

type childModelBinding struct {
	model         ai.LanguageModel
	requestPolicy generation.Policy
}

func newChildModelResolver(
	catalog modelcatalog.Catalog,
	base modelcatalog.ResolvedModel,
	baseModel ai.LanguageModel,
	basePolicy generation.Policy,
	credentials credential.Store,
	decorateModel func(ai.LanguageModel) ai.LanguageModel,
) (childModelResolver, error) {
	if catalog == nil || base.Ref.String() == "" || baseModel == nil || basePolicy == nil {
		return childModelResolver{}, fmt.Errorf("%w: incomplete child model resolver", ErrRuntimeInvalid)
	}
	if baseModel.Provider() != base.Ref.Provider || baseModel.ModelID() != base.Ref.Model {
		return childModelResolver{}, fmt.Errorf("%w: child base model differs from its resolved selection", ErrRuntimeInvalid)
	}

	return childModelResolver{
		catalog:       catalog,
		base:          base.Clone(),
		baseModel:     baseModel,
		basePolicy:    basePolicy,
		credentials:   credentials,
		decorateModel: decorateModel,
	}, nil
}

// planModel resolves a profile spelling into a canonical configured model ID
// without exposing credentials. It also validates request defaults now, so a
// profile that selects an unusable configured model fails before child Session
// creation.
func (r childModelResolver) planModel(selection string) (string, error) {
	resolved, err := r.resolve(selection)
	if err != nil {
		return "", err
	}
	if _, err := generation.Compile(resolved); err != nil {
		return "", fmt.Errorf("%w: configured child model is unavailable", subagent.ErrInvalid)
	}
	if resolved.Ref != r.base.Ref && r.credentials == nil {
		return "", fmt.Errorf("%w: configured child model credentials are unavailable", subagent.ErrInvalid)
	}

	return resolved.Ref.String(), nil
}

// bind creates the model/request-policy pair after a child plan has been
// persisted. The model identity comes only from the frozen plan and is
// re-resolved against the interaction-captured catalog; it never re-reads a
// profile definition.
func (r childModelResolver) bind(
	ctx context.Context,
	planModel string,
) (childModelBinding, error) {
	resolved, err := r.resolve(planModel)
	if err != nil {
		return childModelBinding{}, err
	}
	policy, err := generation.Compile(resolved)
	if err != nil {
		return childModelBinding{}, fmt.Errorf("%w: configured child model is unavailable", subagent.ErrInvalid)
	}
	if resolved.Ref == r.base.Ref {
		return childModelBinding{model: r.baseModel, requestPolicy: r.basePolicy}, nil
	}
	if r.credentials == nil {
		return childModelBinding{}, fmt.Errorf("%w: configured child model credentials are unavailable", subagent.ErrInvalid)
	}

	bound, err := model.New(ctx, resolved, r.credentials)
	if err != nil {
		return childModelBinding{}, fmt.Errorf("%w: open configured child model", subagent.ErrInvalid)
	}
	if r.decorateModel != nil {
		bound = r.decorateModel(bound)
	}
	if bound == nil || bound.Provider() != resolved.Ref.Provider || bound.ModelID() != resolved.Ref.Model {
		return childModelBinding{}, fmt.Errorf("%w: configured child model identity mismatch", subagent.ErrInvalid)
	}

	return childModelBinding{model: bound, requestPolicy: policy}, nil
}

func (r childModelResolver) resolve(selection string) (modelcatalog.ResolvedModel, error) {
	if selection == "inherit" {
		return r.base.Clone(), nil
	}
	ref, err := config.ParseModelRef(selection)
	if err != nil {
		return modelcatalog.ResolvedModel{}, fmt.Errorf("%w: invalid profile model", subagent.ErrInvalid)
	}
	resolved, err := r.catalog.Resolve(modelcatalog.Selection{Ref: ref})
	if err != nil {
		return modelcatalog.ResolvedModel{}, fmt.Errorf("%w: configured child model is unavailable", subagent.ErrInvalid)
	}

	return resolved, nil
}
