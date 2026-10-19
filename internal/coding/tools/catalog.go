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
	tree         *workspace.Tree
	limits       Limits
	patchFault   patchFault
	plan         *PlanFile
	taskLog      *TaskLogRead
	sessionMedia *SessionMediaRead
}

// PlanFile is the injected plan-mode file boundary. The patch tool applies
// changes to this exact file when plan mode admits it.
type PlanFile struct {
	Path     func() string
	Admitted func() bool
	Read     func(context.Context) (string, error)
	Write    func(context.Context, string) error
}

// TaskLogRead is the injected exact-path read boundary for a background task's
// own log. A log lives in the session directory, outside the workspace tree, so
// the read Tool admits it by exact path and reads it under this boundary's own
// byte bound rather than the workspace file bound. The Tool learns nothing about
// directories or task ids: it asks this boundary whether one requested value is
// an admitted log.
type TaskLogRead struct {
	// Admit reports the admitted path for one requested value, or false when the
	// value names no log this session still owns.
	Admit func(string) (string, bool)
	// Bytes bounds an admitted log's size.
	Bytes int64
}

// SessionMediaRead is the injected exact-path read boundary for the current
// session's media files. A session media file lives in the session directory,
// outside the workspace tree, so the read Tool admits it by exact path and
// reads it under this boundary's own byte bound. The Tool learns nothing about
// directories: it asks this boundary whether one requested value is an admitted
// media file. The runtime owns which paths are admitted.
type SessionMediaRead struct {
	// Admit reports the admitted path for one requested value, or false when the
	// value names no media file this session owns.
	Admit func(string) (string, bool)
	// Bytes bounds an admitted media file's size.
	Bytes int64
}

type catalogConfig struct {
	shell          agent.Tool
	monitor        agent.Tool
	kill           KillBackground
	knownIDs       KnownBackgroundIDs
	readBackground ReadBackgroundTasks
	taskLog        *TaskLogRead
	sessionMedia   *SessionMediaRead
	plan           *PlanFile
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

// WithKnownBackgroundIDs adds the enumeration a not-found stop names, so the
// model can recover an id it lost. A catalog built without it reports a bare
// not-found sentence, which is what a caller that keeps no id list still
// wants.
func WithKnownBackgroundIDs(known KnownBackgroundIDs) CatalogOption {
	return func(cfg *catalogConfig) error {
		if known == nil {
			return errors.New("coding tools: nil background id source")
		}

		if cfg.knownIDs != nil {
			return errors.New("coding tools: background id source already configured")
		}

		cfg.knownIDs = known

		return nil
	}
}

// WithReadBackgroundTasks adds the one read operation the task-output and
// blocking-wait Tools route through. A catalog built without it offers neither
// Tool, which is what a capability mode that keeps no registry also keeps no
// read for.
func WithReadBackgroundTasks(read ReadBackgroundTasks) CatalogOption {
	return func(cfg *catalogConfig) error {
		if read == nil {
			return errors.New("coding tools: nil background read")
		}

		if cfg.readBackground != nil {
			return errors.New("coding tools: background read already configured")
		}

		cfg.readBackground = read

		return nil
	}
}

// WithTaskLogRead admits this session's background task logs to the read Tool.
// The boundary resolves one exact path; the Tool never learns the session
// directory, and a value it does not admit is refused as an outside-workspace
// path.
func WithTaskLogRead(read TaskLogRead) CatalogOption {
	return func(cfg *catalogConfig) error {
		if read.Admit == nil || read.Bytes <= 0 {
			return errors.New("coding tools: incomplete task log read boundary")
		}

		if cfg.taskLog != nil {
			return errors.New("coding tools: task log read boundary already configured")
		}

		cloned := read
		cfg.taskLog = &cloned

		return nil
	}
}

// WithSessionMediaRead admits this session's media files to the read Tool. The
// boundary resolves one exact path; the Tool never learns the session
// directory, and a value it does not admit is refused as an outside-workspace
// path. The runtime owns which paths are admitted, so the boundary is the only
// extension point: a catalog built without it offers no session media read.
func WithSessionMediaRead(read SessionMediaRead) CatalogOption {
	return func(cfg *catalogConfig) error {
		if read.Admit == nil || read.Bytes <= 0 {
			return errors.New("coding tools: incomplete session media read boundary")
		}

		if cfg.sessionMedia != nil {
			return errors.New("coding tools: session media read boundary already configured")
		}

		cloned := read
		cfg.sessionMedia = &cloned

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

	service := &service{
		tree: tree, limits: limits, plan: config.plan,
		taskLog: config.taskLog, sessionMedia: config.sessionMedia,
	}
	readTool := agent.Parallel(agent.NewToolParts(
		readName,
		"Read a workspace file: text as a bounded range of lines, a PNG/JPEG/GIF image as an inline image, or a Jupyter notebook as bounded cell text. Also reads a background task's own log by its output_file path and a session media file by its admitted path. Use offset to continue truncated output.",
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
		localEntry(readTool, catalog.RiskRead, "filesystem", "media", "document"),
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
			NewKillBackgroundTool(config.kill, config.knownIDs),
			catalog.RiskPrivileged,
			"process",
			"monitor",
		))
	}

	// The read Tools are registered where the stop Tool is, and only where a
	// registry backs them. They are unmarked so a wait runs as a batch barrier:
	// a blocking read must not overlap the tools of its own batch.
	if config.readBackground != nil {
		entries = append(entries,
			localEntry(
				NewTaskOutputTool(config.readBackground, config.knownIDs),
				catalog.RiskRead,
				"process",
				"monitor",
			),
			localEntry(
				NewWaitTasksTool(config.readBackground, config.knownIDs),
				catalog.RiskRead,
				"process",
				"monitor",
			),
		)
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
