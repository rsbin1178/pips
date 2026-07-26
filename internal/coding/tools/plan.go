package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/catalog"
	"github.com/rsbin/pips/internal/coding/plandoc"
)

const (
	// PlanCatalogID is the exact local provenance admitted by Plan Mode.
	PlanCatalogID = "coding.plan"
	// ReadPlanName is the application-owned Plan document read capability.
	ReadPlanName = "read_plan"
	// WritePlanName is the sole Plan Mode write-risk exception.
	WritePlanName = "write_plan"
)

type planService struct {
	repository plandoc.Repository
	ref        plandoc.Ref
}

type readPlanResult struct {
	Exists   bool   `json:"exists"`
	Revision string `json:"revision,omitempty"`
	Content  string `json:"content,omitempty"`
	Size     int64  `json:"size"`
}

type writePlanArgs struct {
	ExpectedRevision string `json:"expected_revision,omitempty" description:"Revision returned by read_plan. Leave empty only to create the first Plan."`
	Content          string `json:"content" description:"Complete UTF-8 Markdown content for the current Plan."`
}

type writePlanResult struct {
	Created  bool   `json:"created"`
	Revision string `json:"revision"`
	Size     int64  `json:"size"`
}

// NewPlanCatalog returns session-bound Plan document tools. Their schemas
// intentionally contain no filesystem path.
func NewPlanCatalog(
	repository plandoc.Repository,
	ref plandoc.Ref,
) (*catalog.Catalog, error) {
	if repository == nil {
		return nil, errors.New("coding tools: nil Plan document repository")
	}

	service := &planService{repository: repository, ref: ref}
	read := agent.Parallel(agent.NewTool(
		ReadPlanName,
		"Read the current session-bound implementation Plan and its revision.",
		service.readPlan,
	))
	write := agent.NewTool(
		WritePlanName,
		"Atomically replace the complete current session-bound implementation Plan.",
		service.writePlan,
	)

	return catalog.New(
		catalog.Entry{
			Tool: read, Source: catalog.Source{Kind: catalog.SourceLocal, ID: PlanCatalogID},
			Risk: catalog.RiskRead, Tags: []string{tagBuiltin, tagCoding, "plan"},
		},
		catalog.Entry{
			Tool: write, Source: catalog.Source{Kind: catalog.SourceLocal, ID: PlanCatalogID},
			Risk: catalog.RiskWrite, Tags: []string{tagBuiltin, tagCoding, "plan", "mutation"},
		},
	)
}

func (s *planService) readPlan(ctx context.Context, _ struct{}) (string, error) {
	document, err := s.repository.Read(ctx, s.ref)
	if errors.Is(err, plandoc.ErrNotFound) {
		return encodePlanResult(readPlanResult{})
	}

	if err != nil {
		return "", fmt.Errorf("read current Plan: %w", err)
	}

	return encodePlanResult(readPlanResult{
		Exists: true, Revision: document.Revision, Content: document.Content, Size: document.Size,
	})
}

func (s *planService) writePlan(
	ctx context.Context,
	args writePlanArgs,
) (string, error) {
	_, readErr := s.repository.Read(ctx, s.ref)

	created := errors.Is(readErr, plandoc.ErrNotFound)
	if readErr != nil && !created {
		return "", fmt.Errorf("inspect current Plan: %w", readErr)
	}

	document, err := s.repository.Replace(ctx, s.ref, args.ExpectedRevision, args.Content)
	if err != nil {
		return "", fmt.Errorf("write current Plan: %w", err)
	}

	return encodePlanResult(writePlanResult{
		Created: created, Revision: document.Revision, Size: document.Size,
	})
}

func encodePlanResult(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode Plan result: %w", err)
	}

	return string(data), nil
}
