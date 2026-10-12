package tools

import (
	"context"
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
	plan       *PlanFile
}

// PlanFile is the injected plan-mode file boundary. The patch tool applies
// changes to this exact file when plan mode admits it.
type PlanFile struct {
	Path     func() string
	Admitted func() bool
	Read     func(context.Context) (string, error)
	Write    func(context.Context, string) error
}

type catalogConfig struct {
	shell   agent.Tool
	monitor agent.Tool
	kill    KillBackground
	plan    *PlanFile
}

// WithPlanFile admits the plan-mode file as an additional patch target while
// plan mode is active.
func WithPlanFile(file PlanFile) CatalogOption {
	return func(cfg *catalogConfig) error {
		if file.Path == nil || file.Admitted == nil || file.Read == nil || file.Write == nil {
			return errors.New("coding tools: incomplete plan file boundary")
		}

		cloned := file
		cfg.plan = &cloned

		return nil
	}
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

// WithControlledMonitor adds the monitor wrapper returned by
// approval.Controller. A monitor starts a process like shell does, so it is
// offered in exactly the modes that offer shell.
func WithControlledMonitor(tool agent.Tool) CatalogOption {
	return func(cfg *catalogConfig) error {
		if tool == nil {
			return errors.New("coding tools: nil controlled monitor")
		}

		if tool.Decl().Name != monitorName {
			return errors.New("coding tools: controlled monitor has unexpected name")
		}

		if cfg.monitor != nil {
			return errors.New("coding tools: controlled monitor already configured")
		}

		cfg.monitor = tool

		return nil
	}
}

// WithKillBackground adds the one control operation a background-task stop
// routes through. A catalog built without it offers no stop Tool, which is what
// a capability mode that keeps no monitor also keeps no stop for.
func WithKillBackground(kill KillBackground) CatalogOption {
	return func(cfg *catalogConfig) error {
		if kill == nil {
			return errors.New("coding tools: nil background control")
		}

		if cfg.kill != nil {
			return errors.New("coding tools: background control already configured")
		}

		cfg.kill = kill

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

	service := &service{tree: tree, limits: limits, plan: config.plan}
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

	if config.monitor != nil {
		entries = append(entries, localEntry(
			config.monitor,
			catalog.RiskPrivileged,
			"process",
			"monitor",
		))
	}

	if config.kill != nil {
		entries = append(entries, localEntry(
			NewKillBackgroundTool(config.kill),
			catalog.RiskPrivileged,
			"process",
			"monitor",
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
