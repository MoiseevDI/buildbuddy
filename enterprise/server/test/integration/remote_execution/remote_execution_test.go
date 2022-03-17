package remote_execution_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buildbuddy-io/buildbuddy/enterprise/server/scheduling/scheduler_server"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/test/integration/remote_execution/rbetest"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/testutil/buildbuddy_enterprise"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/testutil/testexecutor"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/testutil/testredis"
	"github.com/buildbuddy-io/buildbuddy/server/interfaces"
	"github.com/buildbuddy-io/buildbuddy/server/remote_cache/cachetools"
	"github.com/buildbuddy-io/buildbuddy/server/testutil/testbazel"
	"github.com/buildbuddy-io/buildbuddy/server/testutil/testenv"
	"github.com/buildbuddy-io/buildbuddy/server/testutil/testfs"
	"github.com/buildbuddy-io/buildbuddy/server/util/bazel"
	"github.com/buildbuddy-io/buildbuddy/server/util/status"
	"github.com/buildbuddy-io/buildbuddy/server/util/testing/flags"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	repb "github.com/buildbuddy-io/buildbuddy/proto/remote_execution"
)

func TestSimpleCommandWithNonZeroExitCode(t *testing.T) {
	rbe := rbetest.NewRBETestEnv(t)

	rbe.AddBuildBuddyServer()
	rbe.AddExecutor()

	cmd := rbe.ExecuteCustomCommand("sh", "-c", "echo hello && echo bye >&2 && exit 5")
	res := cmd.Wait()

	assert.Equal(t, 5, res.ExitCode, "exit code should be propagated")
	assert.Equal(t, "hello\n", res.Stdout, "stdout should be propagated")
	assert.Equal(t, "bye\n", res.Stderr, "stderr should be propagated")
}

func TestActionResultCacheWithSuccessfulAction(t *testing.T) {
	rbe := rbetest.NewRBETestEnv(t)

	rbe.AddBuildBuddyServer()
	rbe.AddExecutor()

	platform := &repb.Platform{
		Properties: []*repb.Platform_Property{
			{Name: "OSFamily", Value: runtime.GOOS},
			{Name: "Arch", Value: runtime.GOARCH},
		},
	}
	invocationID := "testabc123"
	cmd := rbe.Execute(&repb.Command{
		Arguments: []string{"echo"},
		Platform:  platform,
	}, &rbetest.ExecuteOpts{
		InvocationID: invocationID,
	})

	res := cmd.Wait()
	assert.Equal(t, 0, res.ExitCode, "exit code should be propagated")

	_, err := rbe.GetActionResultForFailedAction(context.Background(), cmd, invocationID)
	assert.True(t, status.IsNotFoundError(err))
}

func TestActionResultCacheWithFailedAction(t *testing.T) {
	rbe := rbetest.NewRBETestEnv(t)

	rbe.AddBuildBuddyServer()
	rbe.AddExecutor()

	platform := &repb.Platform{
		Properties: []*repb.Platform_Property{
			{Name: "OSFamily", Value: runtime.GOOS},
			{Name: "Arch", Value: runtime.GOARCH},
		},
	}
	invocationID := "testabc123"
	cmd := rbe.Execute(&repb.Command{
		Arguments: []string{"sh", "-c", "echo hello && echo bye >&2 && exit 5"},
		Platform:  platform,
	}, &rbetest.ExecuteOpts{
		InvocationID: invocationID,
	})

	res := cmd.Wait()
	assert.Equal(t, 5, res.ExitCode, "exit code should be propagated")

	ctx := context.Background()
	failedActionResult, err := rbe.GetActionResultForFailedAction(ctx, cmd, invocationID)
	assert.NoError(t, err)
	assert.Equal(t, int32(5), failedActionResult.GetExitCode(), "exit code should be set in action result")
	stdout, stderr, err := rbe.GetStdoutAndStderr(ctx, failedActionResult, res.InstanceName)
	assert.NoError(t, err)
	assert.Equal(t, "hello\n", stdout, "stdout should be propagated")
	assert.Equal(t, "bye\n", stderr, "stderr should be propagated")

	// Verify that it returns NotFound error when looking up action result with unmodified
	// action digest when the action failed.
	_, err = cachetools.GetActionResult(ctx, rbe.GetActionResultStorageClient(), cmd.GetActionResourceName())
	assert.True(t, status.IsNotFoundError(err))
}

func TestSimpleCommandWithZeroExitCode(t *testing.T) {
	rbe := rbetest.NewRBETestEnv(t)

	rbe.AddBuildBuddyServer()
	rbe.AddExecutor()

	cmd := rbe.ExecuteCustomCommand("sh", "-c", "echo hello && echo bye >&2")
	res := cmd.Wait()

	assert.Equal(t, 0, res.ExitCode, "exit code should be propagated")
	assert.Equal(t, "hello\n", res.Stdout, "stdout should be propagated")
	assert.Equal(t, "bye\n", res.Stderr, "stderr should be propagated")
}

func TestSimpleCommand_CommandNotFound_FailedPrecondition(t *testing.T) {
	rbe := rbetest.NewRBETestEnv(t)

	rbe.AddBuildBuddyServer()
	rbe.AddExecutor()

	cmd := rbe.ExecuteCustomCommand("/COMMAND_THAT_DOES_NOT_EXIST")
	err := cmd.MustFail()

	require.Error(t, err)
	assert.True(t, status.IsFailedPreconditionError(err))
	assert.Contains(t, status.Message(err), "no such file or directory")
}

func TestSimpleCommand_Abort_ReturnsExecutionErrorWithoutRetrying(t *testing.T) {
	rbe := rbetest.NewRBETestEnv(t)

	rbe.AddBuildBuddyServer()
	rbe.AddExecutor()

	cmd := rbe.ExecuteCustomCommand("sh", "-c", "kill -ABRT $$")
	// TODO(bduffany): Expect a failed ActionResult here rather than a
	// RESOURCE_EXHAUSTED error, since some tools use abort() as a normal
	// exit condition.
	err := cmd.MustFail()

	assert.True(
		t, status.IsResourceExhaustedError(err),
		"expecting RESOURCE_EXHAUSTED but got: %s", err)
	assert.Contains(t, err.Error(), "signal: aborted")
	assert.NotContains(t, err.Error(), "attempt", "task should not have been retried")
}

func TestSimpleCommandWithExecutorAuthorizationEnabled(t *testing.T) {
	rbe := rbetest.NewRBETestEnv(t)

	rbe.AddBuildBuddyServerWithOptions(&rbetest.BuildBuddyServerOptions{
		SchedulerServerOptions: scheduler_server.Options{
			RequireExecutorAuthorization: true,
		},
	})
	rbe.AddExecutorWithOptions(&rbetest.ExecutorOptions{
		Name:   "executor",
		APIKey: rbetest.ExecutorAPIKey,
	})

	cmd := rbe.ExecuteCustomCommand("sh", "-c", "echo hello && echo bye >&2")
	cmd.Wait()
}

func TestSimpleCommand_RunnerReuse_CanReadPreviouslyWrittenFileButNotOutputDirs(t *testing.T) {
	rbe := rbetest.NewRBETestEnv(t)

	rbe.AddBuildBuddyServer()
	rbe.AddExecutor()

	platform := &repb.Platform{
		Properties: []*repb.Platform_Property{
			{Name: "container-image", Value: "none"},
			{Name: "recycle-runner", Value: "true"},
			{Name: "preserve-workspace", Value: "true"},
			{Name: "OSFamily", Value: runtime.GOOS},
			{Name: "Arch", Value: runtime.GOARCH},
		},
	}

	// Note: authentication is required for workspace reuse, currently.
	opts := &rbetest.ExecuteOpts{UserID: rbe.UserID1}

	cmd := rbe.Execute(&repb.Command{
		Arguments: []string{
			"touch", "output.txt", "undeclared_output.txt", "output_dir/output.txt",
		},
		Platform:          platform,
		OutputDirectories: []string{"output_dir"},
		OutputFiles:       []string{"output.txt"},
	}, opts)
	cmd.Wait()

	cmd = rbe.Execute(&repb.Command{
		Arguments: []string{
			"ls", "output.txt", "undeclared_output.txt", "output_dir/output.txt",
		},
		Platform: platform,
	}, opts)
	res := cmd.Wait()

	assert.Equal(
		t, "undeclared_output.txt\n", res.Stdout,
		"should be able to read undeclared outputs but not other outputs")
}

func TestSimpleCommand_RunnerReuse_ReLinksFilesFromFileCache(t *testing.T) {
	rbe := rbetest.NewRBETestEnv(t)

	rbe.AddBuildBuddyServer()
	rbe.AddExecutor()

	tmpDir := testfs.MakeTempDir(t)
	testfs.WriteAllFileContents(t, tmpDir, map[string]string{
		"f1.input": "A",
		"f2.input": "B",
	})
	platform := &repb.Platform{
		Properties: []*repb.Platform_Property{
			{Name: "container-image", Value: "none"},
			{Name: "recycle-runner", Value: "true"},
			{Name: "preserve-workspace", Value: "true"},
			{Name: "OSFamily", Value: runtime.GOOS},
			{Name: "Arch", Value: runtime.GOARCH},
		},
	}
	opts := &rbetest.ExecuteOpts{InputRootDir: tmpDir, UserID: rbe.UserID1}

	cmd := rbe.Execute(&repb.Command{
		Arguments: []string{"cat", "f1.input", "f2.input"},
		Platform:  platform,
	}, opts)
	res := cmd.Wait()

	require.Equal(t, "AB", res.Stdout)

	tmpDir = testfs.MakeTempDir(t)
	testfs.WriteAllFileContents(t, tmpDir, map[string]string{
		// Overwrite "a.input" with "B" so that we attempt to link over "a.input"
		// from the filecache ("B" should exist in the filecache since it was
		// present as "b.input" in the previous action).
		"f1.input": "B",
	})
	opts = &rbetest.ExecuteOpts{InputRootDir: tmpDir, UserID: rbe.UserID1}

	cmd = rbe.Execute(&repb.Command{
		Arguments: []string{"cat", "f1.input", "f2.input"},
		Platform:  platform,
	}, opts)
	res = cmd.Wait()

	// If this still equals "A" then we probably didn't properly delete
	// "a.input" before linking it from FileCache.
	require.Equal(t, "BB", res.Stdout)
}

func TestSimpleCommand_RunnerReuse_ReLinksFilesFromDuplicateInputs(t *testing.T) {
	rbe := rbetest.NewRBETestEnv(t)

	rbe.AddBuildBuddyServer()
	rbe.AddExecutor()

	tmpDir := testfs.MakeTempDir(t)
	testfs.WriteAllFileContents(t, tmpDir, map[string]string{
		"f1.input": "A",
		"f2.input": "A",
	})
	platform := &repb.Platform{
		Properties: []*repb.Platform_Property{
			{Name: "container-image", Value: "none"},
			{Name: "recycle-runner", Value: "true"},
			{Name: "preserve-workspace", Value: "true"},
			{Name: "OSFamily", Value: runtime.GOOS},
			{Name: "Arch", Value: runtime.GOARCH},
		},
	}
	opts := &rbetest.ExecuteOpts{InputRootDir: tmpDir, UserID: rbe.UserID1}

	cmd := rbe.Execute(&repb.Command{
		Arguments: []string{"cat", "f1.input", "f2.input"},
		Platform:  platform,
	}, opts)
	res := cmd.Wait()

	require.Equal(t, "AA", res.Stdout)

	tmpDir = testfs.MakeTempDir(t)
	testfs.WriteAllFileContents(t, tmpDir, map[string]string{
		"f1.input": "B",
		"f2.input": "B",
	})
	opts = &rbetest.ExecuteOpts{InputRootDir: tmpDir, UserID: rbe.UserID1}

	cmd = rbe.Execute(&repb.Command{
		Arguments: []string{"cat", "f1.input", "f2.input"},
		Platform:  platform,
	}, opts)
	res = cmd.Wait()

	// If either of these is equal to "A" then we didn't properly link the file
	// from its duplicate.
	require.Equal(t, "BB", res.Stdout)
}

func TestSimpleCommand_RunnerReuse_MultipleExecutors_RoutesCommandToSameExecutor(t *testing.T) {
	ctx := context.Background()
	rbe := rbetest.NewRBETestEnv(t)

	rbe.AddBuildBuddyServers(3)
	rbe.AddExecutors(10)

	platform := &repb.Platform{
		Properties: []*repb.Platform_Property{
			{Name: "recycle-runner", Value: "true"},
			{Name: "preserve-workspace", Value: "true"},
			{Name: "OSFamily", Value: runtime.GOOS},
			{Name: "Arch", Value: runtime.GOARCH},
		},
	}
	opts := &rbetest.ExecuteOpts{UserID: rbe.UserID1}

	cmd := rbe.Execute(&repb.Command{
		Arguments: []string{"touch", "foo.txt"},
		Platform:  platform,
	}, opts)
	res := cmd.Wait()

	require.Equal(t, 0, res.ExitCode)

	rbetest.WaitForAnyPooledRunner(t, ctx)

	cmd = rbe.Execute(&repb.Command{
		Arguments: []string{"stat", "foo.txt"},
		Platform:  platform,
	}, opts)
	res = cmd.Wait()

	require.Equal(t, "", res.Stderr)
	require.Equal(t, 0, res.ExitCode)
}

func TestSimpleCommand_RunnerReuse_PoolSelectionViaHeader_RoutesCommandToSameExecutor(t *testing.T) {
	ctx := context.Background()
	rbe := rbetest.NewRBETestEnv(t)

	rbe.AddBuildBuddyServers(3)
	for i := 0; i < 5; i++ {
		rbe.AddExecutorWithOptions(&rbetest.ExecutorOptions{Pool: "foo"})
	}

	platform := &repb.Platform{
		Properties: []*repb.Platform_Property{
			{Name: "recycle-runner", Value: "true"},
			{Name: "preserve-workspace", Value: "true"},
			{Name: "Pool", Value: "THIS_VALUE_SHOULD_BE_OVERRIDDEN"},
			{Name: "OSFamily", Value: runtime.GOOS},
			{Name: "Arch", Value: runtime.GOARCH},
		},
	}
	opts := &rbetest.ExecuteOpts{
		UserID: rbe.UserID1,
		RemoteHeaders: map[string]string{
			"x-buildbuddy-platform.pool": "foo",
		},
	}

	cmd := rbe.Execute(&repb.Command{
		Arguments: []string{"touch", "foo.txt"},
		Platform:  platform,
	}, opts)
	res := cmd.Wait()

	require.Equal(t, 0, res.ExitCode)

	rbetest.WaitForAnyPooledRunner(t, ctx)

	cmd = rbe.Execute(&repb.Command{
		Arguments: []string{"stat", "foo.txt"},
		Platform:  platform,
	}, opts)
	res = cmd.Wait()

	require.Equal(t, "", res.Stderr)
	require.Equal(t, 0, res.ExitCode)
}

func TestSimpleCommandWithMultipleExecutors(t *testing.T) {
	rbe := rbetest.NewRBETestEnv(t)

	rbe.AddBuildBuddyServer()
	rbe.AddExecutors(5)

	cmd := rbe.ExecuteCustomCommand("sh", "-c", "echo hello && echo bye >&2")
	res := cmd.Wait()

	assert.Equal(t, 0, res.ExitCode, "exit code should be propagated")
	assert.Equal(t, "hello\n", res.Stdout, "stdout should be propagated")
	assert.Equal(t, "bye\n", res.Stderr, "stderr should be propagated")
}

func TestSimpleCommandWithPoolSelectionViaPlatformProp_Success(t *testing.T) {
	rbe := rbetest.NewRBETestEnv(t)

	rbe.AddBuildBuddyServer()
	rbe.AddExecutorWithOptions(&rbetest.ExecutorOptions{Pool: "FOO"})

	platform := &repb.Platform{
		Properties: []*repb.Platform_Property{
			{Name: "Pool", Value: "Foo"},
			{Name: "OSFamily", Value: runtime.GOOS},
			{Name: "Arch", Value: runtime.GOARCH},
		},
	}
	opts := &rbetest.ExecuteOpts{}

	cmd := rbe.Execute(&repb.Command{
		Arguments: []string{
			"touch", "output.txt", "undeclared_output.txt", "output_dir/output.txt",
		},
		Platform:          platform,
		OutputDirectories: []string{"output_dir"},
		OutputFiles:       []string{"output.txt"},
	}, opts)
	res := cmd.Wait()

	require.Equal(t, 0, res.ExitCode)
}

func TestSimpleCommandWithPoolSelectionViaPlatformProp_Failure(t *testing.T) {
	rbe := rbetest.NewRBETestEnv(t)

	rbe.AddBuildBuddyServer()
	rbe.AddExecutorWithOptions(&rbetest.ExecutorOptions{Pool: "bar"})

	platform := &repb.Platform{
		Properties: []*repb.Platform_Property{
			{Name: "Pool", Value: "foo"},
			{Name: "OSFamily", Value: runtime.GOOS},
			{Name: "Arch", Value: runtime.GOARCH},
		},
	}
	opts := &rbetest.ExecuteOpts{}

	cmd := rbe.Execute(&repb.Command{
		Arguments: []string{
			"touch", "output.txt", "undeclared_output.txt", "output_dir/output.txt",
		},
		Platform:          platform,
		OutputDirectories: []string{"output_dir"},
		OutputFiles:       []string{"output.txt"},
	}, opts)
	err := cmd.MustFail()

	require.Contains(t, err.Error(), `No registered executors in pool "foo"`)
}

func TestSimpleCommandWithPoolSelectionViaHeader(t *testing.T) {
	rbe := rbetest.NewRBETestEnv(t)

	rbe.AddBuildBuddyServer()
	rbe.AddExecutorWithOptions(&rbetest.ExecutorOptions{Pool: "foo"})
	platform := &repb.Platform{
		Properties: []*repb.Platform_Property{
			{Name: "Pool", Value: "THIS_VALUE_SHOULD_BE_OVERRIDDEN"},
			{Name: "OSFamily", Value: runtime.GOOS},
			{Name: "Arch", Value: runtime.GOARCH},
		},
	}
	opts := &rbetest.ExecuteOpts{
		RemoteHeaders: map[string]string{
			"x-buildbuddy-platform.pool": "foo",
		},
	}

	cmd := rbe.Execute(&repb.Command{
		Arguments: []string{
			"touch", "output.txt", "undeclared_output.txt", "output_dir/output.txt",
		},
		Platform:          platform,
		OutputDirectories: []string{"output_dir"},
		OutputFiles:       []string{"output.txt"},
	}, opts)
	res := cmd.Wait()

	require.Equal(t, 0, res.ExitCode)
}

func TestSimpleCommandWithOSArchPool_CaseInsensitive(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("Test assumed running on linux")
	}
	rbe := rbetest.NewRBETestEnv(t)

	rbe.AddBuildBuddyServer()
	rbe.AddExecutorWithOptions(&rbetest.ExecutorOptions{Pool: "foo"})
	platform := &repb.Platform{
		Properties: []*repb.Platform_Property{
			{Name: "Pool", Value: "FoO"},
			{Name: "OSFamily", Value: "LiNuX"},
			{Name: "Arch", Value: "AmD64"},
		},
	}

	cmd := rbe.Execute(&repb.Command{
		Arguments: []string{"pwd"},
		Platform:  platform,
	}, &rbetest.ExecuteOpts{})
	res := cmd.Wait()

	require.Equal(t, 0, res.ExitCode)
}

func TestSimpleCommand_DefaultWorkspacePermissions(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("Test requires GNU stat")
	}

	rbe := rbetest.NewRBETestEnv(t)
	rbe.AddBuildBuddyServer()
	rbe.AddExecutor()

	inputRoot := testfs.MakeTempDir(t)
	testfs.WriteAllFileContents(t, inputRoot, map[string]string{
		"input_dir/input.txt": "",
	})

	dirs := []string{
		".", "output_dir", "output_dir_parent", "output_dir_parent/output_dir_child",
		"output_file_parent", "input_dir",
	}

	cmd := rbe.Execute(&repb.Command{
		// %a %n prints perms in octal followed by the file name.
		Arguments:         append([]string{"stat", "--format", "%a %n"}, dirs...),
		OutputDirectories: []string{"output_dir", "output_dir_parent/output_dir_child"},
		OutputFiles:       []string{"output_file_parent/output.txt"},
	}, &rbetest.ExecuteOpts{InputRootDir: inputRoot})
	res := cmd.Wait()

	expectedOutput := ""
	for _, dir := range dirs {
		expectedOutput += "755 " + dir + "\n"
	}

	require.Equal(t, expectedOutput, res.Stdout)
}

func TestSimpleCommand_NonrootWorkspacePermissions(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("Test requires GNU stat")
	}

	rbe := rbetest.NewRBETestEnv(t)
	rbe.AddBuildBuddyServer()
	rbe.AddExecutor()

	platform := &repb.Platform{
		Properties: []*repb.Platform_Property{
			{Name: "nonroot-workspace", Value: "true"},
			{Name: "OSFamily", Value: runtime.GOOS},
			{Name: "Arch", Value: runtime.GOARCH},
		},
	}

	inputRoot := testfs.MakeTempDir(t)
	testfs.WriteAllFileContents(t, inputRoot, map[string]string{
		"input_dir/input.txt": "",
	})

	dirs := []string{
		".", "output_dir", "output_dir_parent", "output_dir_parent/output_dir_child",
		"output_file_parent", "input_dir",
	}

	cmd := rbe.Execute(&repb.Command{
		// %a %n prints perms in octal followed by the file name.
		Arguments:         append([]string{"stat", "--format", "%a %n"}, dirs...),
		OutputDirectories: []string{"output_dir", "output_dir_parent/output_dir_child"},
		OutputFiles:       []string{"output_file_parent/output.txt"},
		Platform:          platform,
	}, &rbetest.ExecuteOpts{InputRootDir: inputRoot})
	res := cmd.Wait()

	expectedOutput := ""
	for _, dir := range dirs {
		expectedOutput += "777 " + dir + "\n"
	}

	require.Equal(t, expectedOutput, res.Stdout)
}

func TestManySimpleCommandsWithMultipleExecutors(t *testing.T) {
	rbe := rbetest.NewRBETestEnv(t)

	rbe.AddBuildBuddyServer()
	rbe.AddExecutors(5)

	var cmds []*rbetest.Command
	for i := 0; i < 5; i++ {
		cmd := rbe.ExecuteCustomCommand("sh", "-c", fmt.Sprintf("echo 'hello from command %d'", i))
		cmds = append(cmds, cmd)
	}
	for i := range cmds {
		res := cmds[i].Wait()
		assert.Equal(t, 0, res.ExitCode, "exit code should be propagated")
		assert.Equal(t, fmt.Sprintf("hello from command %d\n", i), res.Stdout, "stdout should be propagated")
		assert.Equal(t, "", res.Stderr, "stderr should be empty")
	}
}

func TestRedisAvailabilityMonitoring(t *testing.T) {
	flags.Set(t, "remote_execution.enable_redis_availability_monitoring", "true")

	rbe := rbetest.NewRBETestEnv(t)

	rbe.AddBuildBuddyServer()
	rbe.AddExecutors(5)

	var cmds []*rbetest.Command
	for i := 0; i < 5; i++ {
		cmd := rbe.ExecuteCustomCommand("sh", "-c", fmt.Sprintf("echo 'hello from command %d'", i))
		cmds = append(cmds, cmd)
	}
	for i := range cmds {
		res := cmds[i].Wait()
		assert.Equal(t, 0, res.ExitCode, "exit code should be propagated")
		assert.Equal(t, fmt.Sprintf("hello from command %d\n", i), res.Stdout, "stdout should be propagated")
		assert.Equal(t, "", res.Stderr, "stderr should be empty")
	}
}

func TestBasicActionIO(t *testing.T) {
	tmpDir := testfs.MakeTempDir(t)
	testfs.WriteAllFileContents(t, tmpDir, map[string]string{
		"greeting.input":       "Hello ",
		"child/farewell.input": "Goodbye ",
	})

	rbe := rbetest.NewRBETestEnv(t)
	rbe.AddBuildBuddyServer()
	rbe.AddExecutor()

	platform := &repb.Platform{
		Properties: []*repb.Platform_Property{
			{Name: "OSFamily", Value: runtime.GOOS},
			{Name: "Arch", Value: runtime.GOARCH},
		},
	}

	opts := &rbetest.ExecuteOpts{InputRootDir: tmpDir}
	cmd := rbe.Execute(&repb.Command{
		Arguments: []string{
			"sh", "-c", strings.Join([]string{
				`set -e`,
				// Create a file in the output directory.
				// No need to create the output directory itself; executor is
				// responsible for that.
				`cp greeting.input out_dir/hello_world.output`,
				`printf 'world' >> out_dir/hello_world.output`,
				// Create a file in a child dir of the output directory.
				// Need to create the child directory ourselves since it's not a declared
				// output directory. Note that the executor should still upload it as
				// part of the output dir tree.
				`mkdir out_dir/child`,
				`cp child/farewell.input out_dir/child/goodbye_world.output`,
				`printf 'world' >> out_dir/child/goodbye_world.output`,
				// Create an explicitly declared output
				`cp greeting.input out_files_dir/hello_bb.output`,
				`printf 'BB' >> out_files_dir/hello_bb.output`,
				// Create another explicitly declared output.
				// No need to create out_files_dir/child; executor is responsible for that.
				`cp child/farewell.input out_files_dir/child/goodbye_bb.output`,
				`printf 'BB' >> out_files_dir/child/goodbye_bb.output`,
			}, "\n"),
		},
		Platform:          platform,
		OutputDirectories: []string{"out_dir"},
		OutputFiles: []string{
			"out_files_dir/hello_bb.output",
			"out_files_dir/child/goodbye_bb.output",
		},
	}, opts)
	res := cmd.Wait()

	require.Equal(t, 0, res.ExitCode)

	outDir := rbe.DownloadOutputsToNewTempDir(res)

	testfs.AssertExactFileContents(t, outDir, map[string]string{
		"out_dir/hello_world.output":            "Hello world",
		"out_dir/child/goodbye_world.output":    "Goodbye world",
		"out_files_dir/hello_bb.output":         "Hello BB",
		"out_files_dir/child/goodbye_bb.output": "Goodbye BB",
	})
}

func TestComplexActionIO(t *testing.T) {
	tmpDir := testfs.MakeTempDir(t)
	// Write a mix of small and large files, to ensure we can handle batching
	// lots of small files that fit within the gRPC limit, as well as individual
	// that exceed the gRPC limit.
	smallSizes := []int{
		1e2, 1e3, 1e4, 1e5, 1e6,
		1e2, 1e3, 1e4, 1e5, 1e6,
	}
	largeSizes := []int{1e7}
	// Write files to several different directories to ensure we handle directory
	// creation properly.
	dirLayout := map[string][]int{
		"" /*root*/ : smallSizes,
		"a":          smallSizes,
		"b":          smallSizes,
		"a/child":    smallSizes,
		"b/child":    largeSizes,
	}
	outputFiles := []string{}
	contents := map[string]string{}
	for dir, sizes := range dirLayout {
		if err := os.MkdirAll(filepath.Join(tmpDir, dir), 0777); err != nil {
			assert.FailNow(t, err.Error())
		}
		for i, size := range sizes {
			relPath := filepath.Join(dir, fmt.Sprintf("file_%d.input", i))
			content := testfs.WriteRandomString(t, tmpDir, relPath, size)
			contents[relPath] = content
			outputFiles = append(outputFiles, filepath.Join("out_files_dir", dir, fmt.Sprintf("file_%d.output", i)))
		}
	}

	rbe := rbetest.NewRBETestEnv(t)
	rbe.AddBuildBuddyServer()
	rbe.AddExecutor()

	opts := &rbetest.ExecuteOpts{InputRootDir: tmpDir}

	platform := &repb.Platform{
		Properties: []*repb.Platform_Property{
			{Name: "OSFamily", Value: runtime.GOOS},
			{Name: "Arch", Value: runtime.GOARCH},
		},
	}
	cmd := rbe.Execute(&repb.Command{
		Arguments: []string{"sh", "-c", strings.Join([]string{
			`set -e`,
			`input_paths=$(find . -type f)`,
			// Mirror the input tree to out_files_dir, skipping the first byte so that
			// the output digests are different. Note that we don't create directories
			// here since the executor is responsible for creating parent dirs of
			// output files.
			`
			for path in $input_paths; do
				output_path="out_files_dir/$(echo "$path" | sed 's/.input/.output/')"
				cat "$path" | tail -c +2 > "$output_path"
			done
			`,
			// Mirror the input tree to out_dir, skipping the first 2 bytes this time.
			// We *do* need to create parent dirs since the executor is only
			// responsible for creating the top-level out_dir.
			`
			for path in $input_paths; do
				output_path="out_dir/$(echo "$path" | sed 's/.input/.output/')"
				mkdir -p out_dir/"$(dirname "$path")"
				cat "$path" | tail -c +3 > "$output_path"
			done
			`,
		}, "\n")},
		Platform:          platform,
		OutputDirectories: []string{"out_dir"},
		OutputFiles:       outputFiles,
	}, opts)
	res := cmd.Wait()

	require.Equal(t, 0, res.ExitCode)
	require.Empty(t, res.Stderr)

	outDir := rbe.DownloadOutputsToNewTempDir(res)

	skippedBytes := map[string]int{
		"out_files_dir": 1,
		"out_dir":       2,
	}
	missing := []string{}
	for parent, nSkippedBytes := range skippedBytes {
		for dir, sizes := range dirLayout {
			for i, _ := range sizes {
				inputRelPath := filepath.Join(dir, fmt.Sprintf("file_%d.input", i))
				outputRelPath := filepath.Join(parent, dir, fmt.Sprintf("file_%d.output", i))
				if testfs.Exists(t, outDir, outputRelPath) {
					inputContents, ok := contents[inputRelPath]
					require.Truef(t, ok, "sanity check: missing input contents of %s", inputRelPath)
					expectedContents := inputContents[nSkippedBytes:]
					actualContents := testfs.ReadFileAsString(t, outDir, outputRelPath)
					// Not using assert.Equal here since the diff can be huge.
					assert.Truef(
						t, expectedContents == actualContents,
						"contents of %s did not match (expected len: %d; actual len: %d)",
						outputRelPath, len(expectedContents), len(actualContents),
					)
				} else {
					missing = append(missing, outputRelPath)
				}
			}
		}
	}
	assert.Empty(t, missing)
}

func TestUnregisterExecutor(t *testing.T) {
	rbe := rbetest.NewRBETestEnv(t)

	rbe.AddBuildBuddyServer()

	// Start with two executors.
	// AddExecutors will block until both are registered.
	executors := rbe.AddExecutors(2)

	// Remove one of the executors.
	// RemoveExecutor will block until the executor is unregistered.
	rbe.RemoveExecutor(executors[0])
}

func TestMultipleSchedulersAndExecutors(t *testing.T) {
	rbe := rbetest.NewRBETestEnv(t)

	// Start with 2 BuildBuddy servers.
	rbe.AddBuildBuddyServer()
	rbe.AddBuildBuddyServer()
	rbe.AddExecutors(5)

	var cmds []*rbetest.Command
	for i := 0; i < 10; i++ {
		cmd := rbe.ExecuteCustomCommand("sh", "-c", fmt.Sprintf("echo 'hello from command %d'", i))
		cmds = append(cmds, cmd)
	}
	for i := range cmds {
		res := cmds[i].Wait()
		assert.Equal(t, 0, res.ExitCode, "exit code should be propagated")
		assert.Equal(t, fmt.Sprintf("hello from command %d\n", i), res.Stdout, "stdout should be propagated")
		assert.Equal(t, "", res.Stderr, "stderr should be empty")
	}
}

func TestWorkSchedulingOnNewExecutor(t *testing.T) {
	rbe := rbetest.NewRBETestEnv(t)

	rbe.AddBuildBuddyServers(5)
	rbe.AddSingleTaskExecutorWithOptions(&rbetest.ExecutorOptions{Name: "busyExecutor1"})
	rbe.AddSingleTaskExecutorWithOptions(&rbetest.ExecutorOptions{Name: "busyExecutor2"})

	// Schedule 2 controlled commands to keep existing executors busy.
	cmd1 := rbe.ExecuteControlledCommand("command1")
	cmd2 := rbe.ExecuteControlledCommand("command2")

	// Wait until both the commands actually start running on the executors.
	cmd1.WaitStarted()
	cmd2.WaitStarted()

	// Schedule some additional commands that existing executors can't take on.
	var cmds []*rbetest.Command
	for i := 0; i < 10; i++ {
		cmd := rbe.ExecuteCustomCommand("sh", "-c", fmt.Sprintf("echo 'hello from command %d'", i))
		cmds = append(cmds, cmd)
	}

	for _, cmd := range cmds {
		cmd.WaitAccepted()
	}

	// Add a new executor that should get assigned the additional tasks.
	rbe.AddExecutorWithOptions(&rbetest.ExecutorOptions{Name: "newExecutor"})

	for i, cmd := range cmds {
		res := cmd.Wait()
		assert.Equal(t, "newExecutor", res.Executor, "[%s] should have been executed on new executor", cmd.Name)
		assert.Equal(t, 0, res.ExitCode, "exit code should be propagated")
		assert.Equal(t, fmt.Sprintf("hello from command %d\n", i), res.Stdout, "stdout should be propagated")
		assert.Equal(t, "", res.Stderr, "stderr should be empty")
	}

	// Allow controlled commands to exit.
	cmd1.Exit(0)
	cmd2.Exit(0)

	cmd1.Wait()
	cmd2.Wait()
}

// Test WaitExecution across different severs.
func TestWaitExecution(t *testing.T) {
	rbe := rbetest.NewRBETestEnv(t)

	// Start multiple servers so that executions are spread out across different servers.
	for i := 0; i < 5; i++ {
		rbe.AddBuildBuddyServer()
	}
	rbe.AddExecutors(5)

	var cmds []*rbetest.ControlledCommand
	for i := 0; i < 10; i++ {
		cmds = append(cmds, rbe.ExecuteControlledCommand(fmt.Sprintf("command%d", i+1)))
	}

	// Wait until all the commands have started running & have been accepted by the server.
	for _, c := range cmds {
		c.WaitStarted()
		c.WaitAccepted()
	}

	// Cancel in-flight Execute requests and call the WaitExecution API.
	for _, c := range cmds {
		c.ReplaceWaitUsingWaitExecutionAPI()
	}

	for i, c := range cmds {
		c.Exit(int32(i))
	}

	for i, cmd := range cmds {
		res := cmd.Wait()
		assert.Equal(t, i, res.ExitCode, "exit code should be propagated for command %q", cmd.Name)
	}
}

type fixedNodeTaskRouter struct {
	mu          sync.Mutex
	executorIDs map[string]struct{}
}

func newFixedNodeTaskRouter(executorIDs []string) *fixedNodeTaskRouter {
	idSet := make(map[string]struct{})
	for _, id := range executorIDs {
		idSet[id] = struct{}{}
	}
	return &fixedNodeTaskRouter{executorIDs: idSet}
}

func (f *fixedNodeTaskRouter) RankNodes(ctx context.Context, cmd *repb.Command, remoteInstanceName string, nodes []interfaces.ExecutionNode) []interfaces.ExecutionNode {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []interfaces.ExecutionNode
	for _, n := range nodes {
		if _, ok := f.executorIDs[n.GetExecutorID()]; ok {
			out = append(out, n)
		}
	}
	return out
}

func (f *fixedNodeTaskRouter) MarkComplete(ctx context.Context, cmd *repb.Command, remoteInstanceName, executorInstanceID string) {
}

func (f *fixedNodeTaskRouter) UpdateSubset(executorIDs []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	idSet := make(map[string]struct{})
	for _, id := range executorIDs {
		idSet[id] = struct{}{}
	}
	f.executorIDs = idSet
}

func TestTaskReservationsNotLostOnExecutorShutdown(t *testing.T) {
	rbe := rbetest.NewRBETestEnv(t)

	var busyExecutorIDs []string
	for i := 1; i <= 3; i++ {
		busyExecutorIDs = append(busyExecutorIDs, fmt.Sprintf("busyExecutor%d", i))
	}

	// Set up the task router to send all reservations to "busy" executors. These executors will queue up tasks but not
	// try to execute any of them.
	taskRouter := newFixedNodeTaskRouter(busyExecutorIDs)
	rbe.AddBuildBuddyServerWithOptions(&rbetest.BuildBuddyServerOptions{EnvModifier: func(env *testenv.TestEnv) {
		env.SetTaskRouter(taskRouter)
	}})

	var busyExecutors []*rbetest.Executor
	for _, id := range busyExecutorIDs {
		e := rbe.AddSingleTaskExecutorWithOptions(&rbetest.ExecutorOptions{Name: id})
		e.ShutdownTaskScheduler()
		busyExecutors = append(busyExecutors, e)
	}
	// Add another executor that should execute all scheduled commands once the "busy" executors are shut down.
	_ = rbe.AddExecutorWithOptions(&rbetest.ExecutorOptions{Name: "newExecutor"})

	// Now schedule some commands. The fake task router will ensure that the reservations only land on "busy"
	// executors.
	var cmds []*rbetest.Command
	for i := 0; i < 10; i++ {
		cmd := rbe.ExecuteCustomCommand("sh", "-c", fmt.Sprintf("echo 'hello from command %d'", i))
		cmds = append(cmds, cmd)
	}
	for _, cmd := range cmds {
		cmd.WaitAccepted()
	}

	// Update the task router to allow tasks to be routed to the non-busy executor.
	taskRouter.UpdateSubset(append(busyExecutorIDs, "newExecutor"))

	// Now shutdown the "busy" executors which should still have all the commands in their queues.
	// During shutdown the tasks should get re-enqueued onto the non-busy executor.
	for _, e := range busyExecutors {
		rbe.RemoveExecutor(e)
	}

	for _, cmd := range cmds {
		res := cmd.Wait()
		assert.Equal(t, "newExecutor", res.Executor, "[%s] should have been executed on new executor", cmd.Name)
	}
}

func TestCommandWithMissingInputRootDigest(t *testing.T) {
	rbe := rbetest.NewRBETestEnv(t)

	rbe.AddBuildBuddyServer()
	rbe.AddExecutor()

	platform := &repb.Platform{
		Properties: []*repb.Platform_Property{
			{Name: "OSFamily", Value: runtime.GOOS},
			{Name: "Arch", Value: runtime.GOARCH},
		},
	}
	cmd := rbe.Execute(&repb.Command{
		Arguments: []string{"echo"},
		Platform:  platform,
	}, &rbetest.ExecuteOpts{SimulateMissingDigest: true})
	err := cmd.MustFail()
	require.Contains(t, err.Error(), "already attempted")
	require.Contains(t, err.Error(), "not found in cache")
}

func TestRedisRestart(t *testing.T) {
	workspaceContents := map[string]string{
		"WORKSPACE": `workspace(name = "integration_test")`,
		"BUILD":     `genrule(name = "hello_txt", outs = ["hello.txt"], cmd_bash = "sleep 5 && echo 'Hello world' > $@")`,
	}

	var redisShards []*testredis.Handle
	for i := 0; i < 4; i++ {
		redisShards = append(redisShards, testredis.StartTCP(t))
	}

	args := []string{
		"--remote_execution.enable_remote_exec=true",
		"--remote_execution.enable_redis_availability_monitoring=true",
	}
	for _, shard := range redisShards {
		args = append(args, "--remote_execution.sharded_redis.shards="+shard.Target)
	}
	app := buildbuddy_enterprise.RunWithConfig(t, buildbuddy_enterprise.NoAuthConfig, args...)

	_ = testexecutor.Run(t,
		"enterprise/server/cmd/executor/executor_/executor",
		[]string{"--executor.app_target=" + app.GRPCAddress()})

	ctx := context.Background()
	ws := testbazel.MakeTempWorkspace(t, workspaceContents)
	buildFlags := []string{"//:hello.txt"}
	buildFlags = append(buildFlags, app.BESBazelFlags()...)
	buildFlags = append(buildFlags, app.RemoteExecutorBazelFlags()...)

	resultCh := make(chan *bazel.InvocationResult)
	go func() {
		resultCh <- testbazel.Invoke(ctx, t, ws, "build", buildFlags...)
	}()

	// Wait for the remote execution to start by looking for the presence of a
	// redis task key on one of the shards. This shard will become the victim.
	deadline := time.Now().Add(10 * time.Second)
	var victimShard *testredis.Handle
	for time.Now().Before(deadline) && victimShard == nil {
		for _, shard := range redisShards {
			if shard.KeyCount("task/*") > 0 {
				if victimShard != nil {
					require.FailNow(t, "multiple redis shards contain task information")
				}
				victimShard = shard
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.NotNil(t, victimShard, "could not find victim shard")

	// Restart the shard containing task information and verify that the Bazel
	// invocation can still finish successfully.
	victimShard.Restart()

	result := <-resultCh

	assert.NoError(t, result.Error)
	assert.Contains(t, result.Stderr, "Build completed successfully")
	require.NotContains(
		t, result.Stderr, "1 remote cache hit",
		"sanity check: initial build shouldn't be cached",
	)
}
