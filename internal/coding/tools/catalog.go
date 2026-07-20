package tools

import (
	"errors"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/catalog"
	"github.com/rsbin/pips/internal/coding/workspace"
)

const localCatalogID = "coding"

type service struct {
	tree       *workspace.Tree
	limits     Limits
	patchFault patchFault
}

// NewCatalog returns the application-owned coding tool catalog.
func NewCatalog(tree *workspace.Tree, limits Limits) (*catalog.Catalog, error) {
	if tree == nil {
		return nil, errors.New("coding tools: nil workspace tree")
	}

	if err := validateLimits(limits); err != nil {
		return nil, err
	}

	service := &service{tree: tree, limits: limits}
	read := agent.Parallel(agent.NewTool(
		readFileName,
		"Read a bounded range of lines from a workspace text file. Use offset to continue truncated output.",
		service.readFile,
	))
	list := agent.Parallel(agent.NewTool(
		listDirName,
		"List one workspace directory in stable name order without recursive traversal.",
		service.listDir,
	))
	find := agent.Parallel(agent.NewTool(
		findFilesName,
		"Find workspace files with a slash-separated glob. Use ** for recursive matching.",
		service.findFiles,
	))
	search := agent.Parallel(agent.NewTool(
		searchTextName,
		"Search bounded workspace text files using literal text or a Go RE2 regular expression.",
		service.searchText,
	))
	patch := agent.NewTool(
		applyPatchName,
		"Apply a strict Add, Update, or Delete patch to workspace files after complete validation.",
		service.applyPatch,
	)

	return catalog.New(
		localEntry(read, catalog.RiskRead, "filesystem"),
		localEntry(list, catalog.RiskRead, "filesystem"),
		localEntry(find, catalog.RiskRead, "filesystem", "search"),
		localEntry(search, catalog.RiskRead, "filesystem", "search"),
		localEntry(patch, catalog.RiskWrite, "filesystem", "mutation"),
	)
}

func localEntry(tool agent.Tool, risk catalog.Risk, tags ...string) catalog.Entry {
	return catalog.Entry{
		Tool: tool,
		Source: catalog.Source{
			Kind: catalog.SourceLocal,
			ID:   localCatalogID,
		},
		Risk: risk,
		Tags: append([]string{"builtin", "coding"}, tags...),
	}
}
