package bare

import (
	"context"
	"flag"

	"github.com/buildbuddy-io/buildbuddy/enterprise/server/remote_execution/commandutil"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/remote_execution/container"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/remote_execution/platform"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/util/oci"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/util/procstats"
	"github.com/buildbuddy-io/buildbuddy/server/interfaces"
	"github.com/buildbuddy-io/buildbuddy/server/util/status"

	repb "github.com/buildbuddy-io/buildbuddy/proto/remote_execution"
	rnpb "github.com/buildbuddy-io/buildbuddy/proto/runner"
)

var (
	bareEnableStats = flag.Bool("executor.bare.enable_stats", false, "Whether to enable stats for bare command execution.")
)

type Opts struct {
	// EnableStats specifies whether to collect stats while the command is
	// in progress.
	EnableStats bool
}

type Provider struct {
}

func (p *Provider) New(ctx context.Context, props *platform.Properties, _ *repb.ScheduledTask, _ *rnpb.RunnerState, _ string) (container.CommandContainer, error) {
	opts := &Opts{
		EnableStats: *bareEnableStats,
	}
	return NewBareCommandContainer(opts), nil
}

// bareCommandContainer executes commands directly, without any isolation
// between containers.
type bareCommandContainer struct {
	opts    *Opts
	WorkDir string
}

func NewBareCommandContainer(opts *Opts) container.CommandContainer {
	return &bareCommandContainer{opts: opts}
}

func (c *bareCommandContainer) Run(ctx context.Context, command *repb.Command, workDir string, creds oci.Credentials) *interfaces.CommandResult {
	return c.exec(ctx, command, workDir, nil /*=stdio*/)
}

func (c *bareCommandContainer) Create(ctx context.Context, workDir string) error {
	c.WorkDir = workDir
	return nil
}

func (c *bareCommandContainer) Exec(ctx context.Context, cmd *repb.Command, stdio *commandutil.Stdio) *interfaces.CommandResult {
	return c.exec(ctx, cmd, c.WorkDir, stdio)
}

func (c *bareCommandContainer) exec(ctx context.Context, cmd *repb.Command, workDir string, stdio *commandutil.Stdio) *interfaces.CommandResult {
	var statsListener procstats.Listener
	if c.opts.EnableStats {
		defer container.Metrics.Unregister(c)
		statsListener = func(stats *repb.UsageStats) {
			container.Metrics.Observe(c, stats)
		}
	}
	return commandutil.Run(ctx, cmd, workDir, statsListener, stdio)
}

func (c *bareCommandContainer) IsImageCached(ctx context.Context) (bool, error) { return false, nil }
func (c *bareCommandContainer) PullImage(ctx context.Context, creds oci.Credentials) error {
	return nil
}
func (c *bareCommandContainer) Start(ctx context.Context) error   { return nil }
func (c *bareCommandContainer) Remove(ctx context.Context) error  { return nil }
func (c *bareCommandContainer) Pause(ctx context.Context) error   { return nil }
func (c *bareCommandContainer) Unpause(ctx context.Context) error { return nil }

func (c *bareCommandContainer) Stats(ctx context.Context) (*repb.UsageStats, error) {
	return nil, nil
}

func (c *bareCommandContainer) State(ctx context.Context) (*rnpb.ContainerState, error) {
	return nil, status.UnimplementedError("not implemented")
}
