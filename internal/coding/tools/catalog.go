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

type catalogConfig struct {
	shell agent.Tool
}

// CatalogOption adds an explicitly constructed coding tool.
type CatalogOption func(*catalogConfig) error

// WithControlledShell adds the shell wrapper returned by approval.Controller.
// Passing the raw ShellHandler is impossible because it does not implement
// agent.Tool.
func WithControlledShell(tool agent.Tool) CatalogOption {
	return func(cfg *catalogConfig) error {
		if tool == nil {
			return errors.New("coding tools: nil controlled shell")
		}

		if tool.Decl().Name != shellName {
			return errors.New("coding tools: controlled shell has unexpected name")
		}

		if cfg.shell != nil {
			return errors.New("coding tools: controlled shell already configured")
		}

		cfg.shell = tool

		return nil
	}
}

// NewCatalog returns the application-owned coding tool catalog.
func NewCatalog(
	tree *workspace.Tree,
	limits Limits,
	options ...CatalogOption,
) (*catalog.Catalog, error) {
	if tree == nil {
		return nil, errors.New("coding tools: nil workspace tree")
	}

	if err := validateLimits(limits); err != nil {
		return nil, err
	}

	config := catalogConfig{}

	for _, option := range options {
		if option == nil {
			return nil, errors.New("coding tools: nil catalog option")
		}

		if err := option(&config); err != nil {
			return nil, err
		}
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

	entries := []catalog.Entry{
		localEntry(read, catalog.RiskRead, "filesystem"),
		localEntry(list, catalog.RiskRead, "filesystem"),
		localEntry(find, catalog.RiskRead, "filesystem", "search"),
		localEntry(search, catalog.RiskRead, "filesystem", "search"),
		localEntry(patch, catalog.RiskWrite, "filesystem", "mutation"),
	}
	if config.shell != nil {
		entries = append(entries, localEntry(
			config.shell,
			catalog.RiskPrivileged,
			"process",
			"shell",
		))
	}

	return catalog.New(entries...)
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
