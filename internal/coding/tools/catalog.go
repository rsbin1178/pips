package tools

import (
	"errors"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/internal/coding/workspace"
)

const (
	localCatalogID = "coding"
	tagBuiltin     = "builtin"
	tagCoding      = "coding"
)

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
	readTool := agent.Parallel(agent.NewTool(
		readName,
		"Read a bounded range of lines from a workspace text file. Use offset to continue truncated output.",
		service.read,
	))
	lsTool := agent.Parallel(agent.NewTool(
		lsName,
		"List one workspace directory in stable name order without recursive traversal.",
		service.ls,
	))
	globTool := agent.Parallel(agent.NewTool(
		globName,
		"Find workspace files with a slash-separated glob. Use ** for recursive matching.",
		service.glob,
	))
	grepTool := agent.Parallel(agent.NewTool(
		grepName,
		"Search bounded workspace text files using a Go RE2 regular expression. Set fixed_strings for literal matching.",
		service.grep,
	))
	patch := agent.NewTool(
		applyPatchName,
		"Apply a strict Add, Update, or Delete patch to workspace files after complete validation.",
		service.applyPatch,
	)

	entries := []catalog.Entry{
		localEntry(readTool, catalog.RiskRead, "filesystem"),
		localEntry(lsTool, catalog.RiskRead, "filesystem"),
		localEntry(globTool, catalog.RiskRead, "filesystem", "search"),
		localEntry(grepTool, catalog.RiskRead, "filesystem", "search"),
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
		Tags: append([]string{tagBuiltin, tagCoding}, tags...),
	}
}
