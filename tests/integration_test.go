package tests

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

//go:embed testdata/vms.toml
var defaultVmsToml []byte

//go:embed testdata/vms_two.toml
var twoVmsToml []byte

//go:embed testdata/vms_tagged.toml
var taggedVmsToml []byte

//go:embed testdata/tests.toml
var defaultTestsToml []byte

//go:embed testdata/tests_two.toml
var twoTestsToml []byte

//go:embed testdata/tests_multi_vmcount.toml
var multiVmcountTestsToml []byte

//go:embed testdata/tests_variants.toml
var variantsTestsToml []byte

//go:embed testdata/tests_needallplatforms.toml
var needAllPlatformsTestsToml []byte

//go:embed testdata/tests_samevms.toml
var sameVmsTestsToml []byte

//go:embed testdata/tests_vm_tags.toml
var vmTagsTestsToml []byte

//go:embed testdata/tests_many.toml
var manyTestsToml []byte

//go:embed testdata/tests_shared_disks.toml
var sharedDisksTestsToml []byte

//go:embed testdata/tests_disks.toml
var disksTestsToml []byte

//go:embed testdata/vms_disks.toml
var disksVmsToml []byte

//go:embed testdata/tests_resources.toml
var resourcesTestsToml []byte

//go:embed testdata/vms_provision.toml
var provisionVmsToml []byte

//go:embed testdata/vms_tiny.toml
var tinyVmsToml []byte

type vmshedOpts struct {
	VmsToml       []byte
	TestsToml     []byte
	ProvisionToml []byte
	VirterFailOn  string
	VirterDelayOn string
	VirterDelay   string
	ExtraArgs     []string
	ExitCode      int
	// OmitVMCount leaves out the "--nvms 1" argument passed by default.
	OmitVMCount bool
	// OmitResourceLimits leaves out the "--max-vcpus 0 --max-memory 0"
	// arguments passed by default, so the host resources apply.
	OmitResourceLimits bool
}

type virterCall struct {
	Args []string `json:"args"`
}

func (c virterCall) Subcommand() string {
	if len(c.Args) >= 2 {
		return c.Args[0] + " " + c.Args[1]
	}
	return ""
}

func (c virterCall) TestName() string {
	for i, arg := range c.Args {
		if arg == "--set" && i+1 < len(c.Args) {
			if name, ok := strings.CutPrefix(c.Args[i+1], "env.TEST_NAME="); ok {
				return name
			}
		}
	}
	return ""
}

type testResult struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Status     string   `json:"status"`
	VMCount    int      `json:"vm_count"`
	Variant    string   `json:"variant"`
	BaseImages []string `json:"base_images"`
}

type vmshedResult struct {
	ExitCode    int
	Stdout      string
	Stderr      string
	OutDir      string
	VirterCalls []virterCall
	Results     []testResult
}

func vmshedBinary(t *testing.T) string {
	t.Helper()
	p := os.Getenv("VMSHED_BINARY")
	if p == "" {
		t.Skip("VMSHED_BINARY not set")
	}
	return p
}

func mockVirterBinary(t *testing.T) string {
	t.Helper()
	p := os.Getenv("MOCK_VIRTER_BINARY")
	if p == "" {
		t.Skip("MOCK_VIRTER_BINARY not set")
	}
	return p
}

// runVmshed creates config files and runs vmshed as a subprocess
// with the mock virter on PATH.
func runVmshed(t *testing.T, opts vmshedOpts) vmshedResult {
	t.Helper()

	dir := t.TempDir()

	vmsToml := filepath.Join(dir, "vms.toml")
	require.NoError(t, os.WriteFile(vmsToml, opts.VmsToml, 0644))

	testsToml := filepath.Join(dir, "tests.toml")
	require.NoError(t, os.WriteFile(testsToml, opts.TestsToml, 0644))

	runToml := filepath.Join(dir, "run.toml")
	require.NoError(t, os.WriteFile(runToml, []byte(""), 0644))

	if opts.ProvisionToml != nil {
		provisionToml := filepath.Join(dir, "provision.toml")
		require.NoError(t, os.WriteFile(provisionToml, opts.ProvisionToml, 0644))
	}

	// Create a bin directory with mock virter symlinked as "virter"
	binDir := filepath.Join(dir, "bin")
	require.NoError(t, os.Mkdir(binDir, 0755))
	require.NoError(t, os.Symlink(mockVirterBinary(t), filepath.Join(binDir, "virter")))

	outDir := filepath.Join(dir, "out")

	args := []string{
		"--vms", vmsToml,
		"--tests", testsToml,
		"--out-dir", outDir,
		"--startvm", "2",
		"--seed", "1",
	}
	if !opts.OmitVMCount {
		args = append(args, "--nvms", "1")
	}
	if !opts.OmitResourceLimits {
		// Explicit limits in ExtraArgs take precedence over these.
		args = append(args, "--max-vcpus", "0", "--max-memory", "0")
	}
	args = append(args, opts.ExtraArgs...)

	virterLog := filepath.Join(dir, "virter.log")

	cmd := exec.Command(vmshedBinary(t), args...)
	cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))
	cmd.Env = append(cmd.Env, "MOCK_VIRTER_LOG="+virterLog)
	if opts.VirterFailOn != "" {
		cmd.Env = append(cmd.Env, "MOCK_VIRTER_FAIL_ON="+opts.VirterFailOn)
	}
	if opts.VirterDelayOn != "" {
		cmd.Env = append(cmd.Env, "MOCK_VIRTER_DELAY_ON="+opts.VirterDelayOn)
		cmd.Env = append(cmd.Env, "MOCK_VIRTER_DELAY="+opts.VirterDelay)
	}
	cmd.Dir = dir

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("failed to run vmshed: %v", err)
		}
	}

	var calls []virterCall
	if logData, readErr := os.ReadFile(virterLog); readErr == nil {
		dec := json.NewDecoder(strings.NewReader(string(logData)))
		for dec.More() {
			var c virterCall
			require.NoError(t, dec.Decode(&c))
			calls = append(calls, c)
		}
	}

	logVirterCalls(t, calls)
	t.Logf("stderr:\n%s", stderr.String())
	assert.Equal(t, opts.ExitCode, exitCode, "unexpected exit code")

	var results []testResult
	resultsPath := filepath.Join(outDir, "results.json")
	if resultsData, readErr := os.ReadFile(resultsPath); readErr == nil {
		dec := json.NewDecoder(strings.NewReader(string(resultsData)))
		for dec.More() {
			var r testResult
			require.NoError(t, dec.Decode(&r))
			results = append(results, r)
		}
	}

	return vmshedResult{
		ExitCode:    exitCode,
		Stdout:      stdout.String(),
		Stderr:      stderr.String(),
		OutDir:      outDir,
		VirterCalls: calls,
		Results:     results,
	}
}

func logVirterCalls(t *testing.T, calls []virterCall) {
	t.Helper()
	var b strings.Builder
	fmt.Fprintln(&b, "virter calls:")
	for _, c := range calls {
		fmt.Fprintf(&b, "  virter %s\n", strings.Join(c.Args, " "))
	}
	t.Log(b.String())
}

func subcommands(calls []virterCall) []string {
	var subcmds []string
	for _, c := range calls {
		if s := c.Subcommand(); s != "" {
			subcmds = append(subcmds, s)
		}
	}
	return subcmds
}

func countSubcommand(calls []virterCall, subcmd string) int {
	n := 0
	for _, c := range calls {
		if c.Subcommand() == subcmd {
			n++
		}
	}
	return n
}

// maxConcurrentVMs returns the largest number of VMs that were running at
// the same time, judging by the order of the "vm run" and "vm rm" calls.
func maxConcurrentVMs(calls []virterCall) int {
	running := map[string]bool{}
	maxRunning := 0
	for _, c := range calls {
		switch c.Subcommand() {
		case "vm run":
			if i := slices.Index(c.Args, "--name"); i >= 0 && i+1 < len(c.Args) {
				running[c.Args[i+1]] = true
			}
		case "vm rm":
			for _, name := range c.Args[2:] {
				delete(running, name)
			}
		}
		maxRunning = max(maxRunning, len(running))
	}
	return maxRunning
}

// consoleTestID returns the test run ID of a "vm run" call, derived from
// the console directory.
func consoleTestID(c virterCall) string {
	if i := slices.Index(c.Args, "--console"); i >= 0 && i+1 < len(c.Args) {
		return filepath.Base(c.Args[i+1])
	}
	return ""
}

func resultsByName(results []testResult, name string) []testResult {
	var out []testResult
	for _, r := range results {
		if r.Name == name {
			out = append(out, r)
		}
	}
	return out
}

func TestSimpleSuccess(t *testing.T) {
	res := runVmshed(t, vmshedOpts{
		VmsToml:   defaultVmsToml,
		TestsToml: defaultTestsToml,
	})

	expected := []string{"image ls", "network add", "vm rm", "vm run", "vm exec", "vm rm", "network rm"}
	assert.Equal(t, expected, subcommands(res.VirterCalls))

	require.Len(t, res.Results, 1)
	assert.Equal(t, "SUCCESS", res.Results[0].Status)
}

func TestProvision(t *testing.T) {
	res := runVmshed(t, vmshedOpts{
		VmsToml:       provisionVmsToml,
		TestsToml:     defaultTestsToml,
		ProvisionToml: []byte("[[steps]]\n"),
	})

	require.Equal(t, 1, countSubcommand(res.VirterCalls, "image build"))
	for _, c := range res.VirterCalls {
		if c.Subcommand() == "image build" {
			assert.NotContains(t, c.Args, "--push-to")
			assert.NotContains(t, c.Args, "--build-id")
			assert.Equal(t, "testimage-mysuite", c.Args[len(c.Args)-1])
		}
	}

	require.Len(t, res.Results, 1)
	assert.Equal(t, "SUCCESS", res.Results[0].Status)
}

func TestProvisionWithPushTemplate(t *testing.T) {
	res := runVmshed(t, vmshedOpts{
		VmsToml:       provisionVmsToml,
		TestsToml:     defaultTestsToml,
		ProvisionToml: []byte("[[steps]]\n"),
		ExtraArgs:     []string{"--push-template", "registry.example.com/vmshed/{{ .Image }}:latest"},
	})

	require.Equal(t, 1, countSubcommand(res.VirterCalls, "image build"))
	for _, c := range res.VirterCalls {
		if c.Subcommand() != "image build" {
			continue
		}

		pushToIndex := slices.Index(c.Args, "--push-to")
		require.GreaterOrEqual(t, pushToIndex, 0, "image build should push the image")
		require.Less(t, pushToIndex+1, len(c.Args))
		assert.Equal(t, "registry.example.com/vmshed/testimage-mysuite:latest", c.Args[pushToIndex+1])

		buildIDIndex := slices.Index(c.Args, "--build-id")
		require.GreaterOrEqual(t, buildIDIndex, 0, "image build should set a build ID")
		require.Less(t, buildIDIndex+1, len(c.Args))
		assert.Regexp(t, "^[0-9a-f]{64}$", c.Args[buildIDIndex+1])

		assert.Equal(t, "testimage-mysuite", c.Args[len(c.Args)-1],
			"the local image name should not be affected by the push template")
	}

	require.Len(t, res.Results, 1)
	assert.Equal(t, "SUCCESS", res.Results[0].Status)
}

func TestProvisionBuildIDCoverage(t *testing.T) {
	provisionToml := []byte("version = 1\n\n[values]\nDeclared = \"default\"\n\n[[steps]]\n")

	buildID := func(extraSets ...string) string {
		t.Helper()

		args := []string{"--push-template", "registry.example.com/vmshed/{{ .Image }}:latest"}
		for _, set := range extraSets {
			args = append(args, "--set", set)
		}

		res := runVmshed(t, vmshedOpts{
			VmsToml:       provisionVmsToml,
			TestsToml:     defaultTestsToml,
			ProvisionToml: provisionToml,
			ExtraArgs:     args,
		})

		for _, c := range res.VirterCalls {
			if c.Subcommand() != "image build" {
				continue
			}
			buildIDIndex := slices.Index(c.Args, "--build-id")
			require.GreaterOrEqual(t, buildIDIndex, 0)
			require.Less(t, buildIDIndex+1, len(c.Args))
			return c.Args[buildIDIndex+1]
		}

		t.Fatal("no image build call found")
		return ""
	}

	base := buildID()
	assert.Equal(t, base, buildID("values.TestOnly=1"),
		"overrides of undeclared values should not invalidate the cache")
	assert.NotEqual(t, base, buildID("values.Declared=changed"),
		"overrides of declared values should invalidate the cache")
}

func TestSharedDisks(t *testing.T) {
	res := runVmshed(t, vmshedOpts{
		VmsToml:   defaultVmsToml,
		TestsToml: sharedDisksTestsToml,
	})

	expected := []string{"image ls", "network add", "disk rm", "disk create", "vm rm", "vm run", "vm exec", "vm rm", "disk rm", "network rm"}
	assert.Equal(t, expected, subcommands(res.VirterCalls))

	for _, call := range res.VirterCalls {
		switch call.Subcommand() {
		case "disk create":
			assert.Equal(t, []string{"disk", "create", "lbtest-disk-mytest-1-default-0-shared0", "--size", "1G"}, call.Args)
		case "vm run":
			assert.Contains(t, call.Args, "--shared-disk")
			assert.Contains(t, call.Args, "name=lbtest-disk-mytest-1-default-0-shared0,bus=scsi")
		}
	}

	require.Len(t, res.Results, 1)
	assert.Equal(t, "SUCCESS", res.Results[0].Status)
}

func TestSharedDiskCreateFail(t *testing.T) {
	res := runVmshed(t, vmshedOpts{
		VmsToml:      defaultVmsToml,
		TestsToml:    sharedDisksTestsToml,
		VirterFailOn: "disk create",
		ExitCode:     1,
	})

	// The shared disk is removed even though the test never started,
	// and no VM is ever run.
	expected := []string{"image ls", "network add", "disk rm", "disk create", "disk rm", "network rm"}
	assert.Equal(t, expected, subcommands(res.VirterCalls))

	require.Len(t, res.Results, 1)
	assert.Equal(t, "ERROR", res.Results[0].Status)
}

// vmRunDisks returns the values of all "--disk" arguments of "vm run" calls.
func vmRunDisks(calls []virterCall) []string {
	var disks []string
	for _, c := range calls {
		if c.Subcommand() != "vm run" {
			continue
		}
		for i, arg := range c.Args {
			if arg == "--disk" && i+1 < len(c.Args) {
				disks = append(disks, c.Args[i+1])
			}
		}
	}
	return disks
}

func TestDefaultDisk(t *testing.T) {
	res := runVmshed(t, vmshedOpts{
		VmsToml:   defaultVmsToml,
		TestsToml: defaultTestsToml,
	})

	assert.Equal(t, []string{"name=data,size=2G,bus=scsi"}, vmRunDisks(res.VirterCalls))
}

func TestVMDisks(t *testing.T) {
	res := runVmshed(t, vmshedOpts{
		VmsToml:   disksVmsToml,
		TestsToml: defaultTestsToml,
	})

	assert.Equal(t, []string{"name=data,size=5G"}, vmRunDisks(res.VirterCalls))
}

func TestTestDisks(t *testing.T) {
	res := runVmshed(t, vmshedOpts{
		VmsToml:   disksVmsToml,
		TestsToml: disksTestsToml,
	})

	// The disks of the test replace the disks of the VM specification.
	assert.Equal(t, []string{"name=data,size=10G,bus=scsi", "name=meta,size=1G"}, vmRunDisks(res.VirterCalls))

	require.Len(t, res.Results, 1)
	assert.Equal(t, "SUCCESS", res.Results[0].Status)
}

func TestPerTestResources(t *testing.T) {
	res := runVmshed(t, vmshedOpts{
		VmsToml:   defaultVmsToml,
		TestsToml: resourcesTestsToml,
	})

	require.Len(t, res.Results, 2)
	for _, r := range res.Results {
		assert.Equal(t, "SUCCESS", r.Status)
	}

	vmRuns := map[string][]string{}
	for _, c := range res.VirterCalls {
		if c.Subcommand() == "vm run" {
			vmRuns[consoleTestID(c)] = c.Args
		}
	}
	require.Contains(t, vmRuns, "bigtest-1-default-0")
	require.Contains(t, vmRuns, "smalltest-1-default-0")

	big := strings.Join(vmRuns["bigtest-1-default-0"], " ")
	assert.Contains(t, big, "--memory 8G", "the test override should be passed to virter")
	assert.Contains(t, big, "--vcpus 8")

	small := strings.Join(vmRuns["smalltest-1-default-0"], " ")
	assert.Contains(t, small, "--memory 4G", "without override the default applies")
	assert.Contains(t, small, "--vcpus 4")
}

func TestMaxMemorySerializesTests(t *testing.T) {
	// Both tests need 4G, so with a limit of 4G they run one after the other
	// even though the IDs would allow both at once.
	res := runVmshed(t, vmshedOpts{
		VmsToml:       defaultVmsToml,
		TestsToml:     twoTestsToml,
		VirterDelayOn: "vm exec",
		VirterDelay:   "1s",
		ExtraArgs:     []string{"--nvms", "2", "--max-memory", "4G"},
	})

	require.Len(t, res.Results, 2)
	for _, r := range res.Results {
		assert.Equal(t, "SUCCESS", r.Status)
	}
	assert.Equal(t, 1, maxConcurrentVMs(res.VirterCalls),
		"only one 4G test should run at a time with a 4G limit")
}

func TestMaxMemoryAllowsParallelTests(t *testing.T) {
	res := runVmshed(t, vmshedOpts{
		VmsToml:       defaultVmsToml,
		TestsToml:     twoTestsToml,
		VirterDelayOn: "vm exec",
		VirterDelay:   "1s",
		ExtraArgs:     []string{"--nvms", "2", "--max-memory", "8G", "--max-vcpus", "8"},
	})

	require.Len(t, res.Results, 2)
	for _, r := range res.Results {
		assert.Equal(t, "SUCCESS", r.Status)
	}
	assert.Equal(t, 2, maxConcurrentVMs(res.VirterCalls),
		"two 4G tests should run at once with an 8G limit")
}

func TestMemoryOverheadSerializesTests(t *testing.T) {
	// Two 4G tests would fit into 9G, but not with 1G overhead per VM, nor
	// with 2G of the limit reserved.
	for _, extraArgs := range [][]string{
		{"--vm-memory-overhead", "1G"},
		{"--reserved-memory", "2G"},
	} {
		res := runVmshed(t, vmshedOpts{
			VmsToml:       defaultVmsToml,
			TestsToml:     twoTestsToml,
			VirterDelayOn: "vm exec",
			VirterDelay:   "1s",
			ExtraArgs:     append([]string{"--nvms", "2", "--max-memory", "9G", "--max-vcpus", "8"}, extraArgs...),
		})

		require.Len(t, res.Results, 2)
		for _, r := range res.Results {
			assert.Equal(t, "SUCCESS", r.Status)
		}
		assert.Equal(t, 1, maxConcurrentVMs(res.VirterCalls),
			"only one 4G test should run at a time with %v", extraArgs)
	}
}

func TestResourceLimitWithoutVMCount(t *testing.T) {
	// Without --nvms, the resource limit alone decides how many tests run
	// in parallel. Two 4G VMs fit into 8G, so the access network is created
	// with two DHCP entries rather than the default of twelve.
	res := runVmshed(t, vmshedOpts{
		VmsToml:       defaultVmsToml,
		TestsToml:     twoTestsToml,
		VirterDelayOn: "vm exec",
		VirterDelay:   "1s",
		ExtraArgs:     []string{"--max-memory", "8G"},
		OmitVMCount:   true,
	})

	require.Len(t, res.Results, 2)
	for _, r := range res.Results {
		assert.Equal(t, "SUCCESS", r.Status)
	}
	assert.Equal(t, 2, maxConcurrentVMs(res.VirterCalls))

	for _, c := range res.VirterCalls {
		if c.Subcommand() != "network add" {
			continue
		}
		args := strings.Join(c.Args, " ")
		assert.Contains(t, args, "--dhcp-count 2", "the VM count should be derived from the limit")
	}
}

func TestExplicitVMCountWithResourceLimit(t *testing.T) {
	// An explicit --nvms still applies in addition to the resource limit.
	res := runVmshed(t, vmshedOpts{
		VmsToml:       defaultVmsToml,
		TestsToml:     twoTestsToml,
		VirterDelayOn: "vm exec",
		VirterDelay:   "1s",
		ExtraArgs:     []string{"--nvms", "1", "--max-memory", "8G"},
		OmitVMCount:   true,
	})

	require.Len(t, res.Results, 2)
	assert.Equal(t, 1, maxConcurrentVMs(res.VirterCalls))
}

func TestResourceLimitDefaultsToHost(t *testing.T) {
	// Without explicit limits, the CPUs and memory of the host apply. A
	// tiny VM fits on any host, so the test runs and the limits are logged.
	res := runVmshed(t, vmshedOpts{
		VmsToml:            tinyVmsToml,
		TestsToml:          defaultTestsToml,
		OmitResourceLimits: true,
	})

	require.Len(t, res.Results, 1)
	assert.Equal(t, "SUCCESS", res.Results[0].Status)
	assert.Regexp(t, `Resource limit: [0-9]+ vCPUs, [0-9]+[BKMGTPE] memory`, res.Stderr,
		"both limits should be discovered from the host")
}

func TestMaxMemoryTooSmall(t *testing.T) {
	res := runVmshed(t, vmshedOpts{
		VmsToml:   defaultVmsToml,
		TestsToml: defaultTestsToml,
		ExtraArgs: []string{"--max-memory", "2G"},
		ExitCode:  1,
	})

	assert.Equal(t, 0, countSubcommand(res.VirterCalls, "vm run"),
		"a test that can never fit the limit should fail before anything runs")
	assert.Contains(t, res.Stderr, "exceed the limit")
	assert.Contains(t, res.Stderr, "mytest-1-default-0")
	assert.Empty(t, res.Results)
}

func TestMultipleTestsSuccess(t *testing.T) {
	res := runVmshed(t, vmshedOpts{
		VmsToml:   defaultVmsToml,
		TestsToml: twoTestsToml,
	})

	require.Len(t, res.Results, 2)
	for _, r := range res.Results {
		assert.Equal(t, "SUCCESS", r.Status)
	}
	assert.Equal(t, 2, countSubcommand(res.VirterCalls, "vm exec"))
}

func TestSimpleFailure(t *testing.T) {
	res := runVmshed(t, vmshedOpts{
		VmsToml:      defaultVmsToml,
		TestsToml:    defaultTestsToml,
		VirterFailOn: "vm exec",
		ExitCode:     1,
	})

	assert.Contains(t, subcommands(res.VirterCalls), "vm exec", "vm exec should have been attempted")

	require.Len(t, res.Results, 1)
	assert.Equal(t, "FAILED", res.Results[0].Status)
}

func TestOnFailureContinue(t *testing.T) {
	res := runVmshed(t, vmshedOpts{
		VmsToml:      defaultVmsToml,
		TestsToml:    twoTestsToml,
		VirterFailOn: "vm exec",
		ExitCode:     1,
	})

	require.Len(t, res.Results, 2)
	for _, r := range res.Results {
		assert.Equal(t, "FAILED", r.Status)
	}
	assert.Equal(t, 2, countSubcommand(res.VirterCalls, "vm exec"))
}

func TestOnFailureTerminate(t *testing.T) {
	res := runVmshed(t, vmshedOpts{
		VmsToml:      defaultVmsToml,
		TestsToml:    twoTestsToml,
		VirterFailOn: "vm exec",
		ExtraArgs:    []string{"--on-failure", "terminate"},
		ExitCode:     1,
	})

	// Only one test should have run; the other should have been skipped
	assert.Equal(t, 1, countSubcommand(res.VirterCalls, "vm exec"), "only one vm exec should have been attempted")

	require.Len(t, res.Results, 1)
	assert.Equal(t, "FAILED", res.Results[0].Status)
}

func TestOnFailureKeepVms(t *testing.T) {
	res := runVmshed(t, vmshedOpts{
		VmsToml:      defaultVmsToml,
		TestsToml:    defaultTestsToml,
		VirterFailOn: "vm exec",
		ExtraArgs:    []string{"--on-failure", "keep-vms"},
		ExitCode:     1,
	})

	require.Len(t, res.Results, 1)
	assert.Equal(t, "FAILED", res.Results[0].Status)
	assert.NotContains(t, subcommands(res.VirterCalls), "network rm")
}

func TestToRunFilter(t *testing.T) {
	res := runVmshed(t, vmshedOpts{
		VmsToml:   defaultVmsToml,
		TestsToml: twoTestsToml,
		ExtraArgs: []string{"--torun", "first"},
	})

	require.Len(t, res.Results, 1)
	assert.Equal(t, "first", res.Results[0].Name)
}

func TestBaseImageFilter(t *testing.T) {
	res := runVmshed(t, vmshedOpts{
		VmsToml:   twoVmsToml,
		TestsToml: needAllPlatformsTestsToml,
		ExtraArgs: []string{"--base-image", "imageA", "--repeats", "10"},
	})

	require.Len(t, res.Results, 10)
	for _, r := range res.Results {
		require.Len(t, r.BaseImages, 1)
		assert.Equal(t, "imageA", r.BaseImages[0])
	}
}

func TestVariantFilter(t *testing.T) {
	res := runVmshed(t, vmshedOpts{
		VmsToml:   defaultVmsToml,
		TestsToml: variantsTestsToml,
		ExtraArgs: []string{"--variant", "beta"},
	})

	require.Len(t, res.Results, 1)
	assert.Equal(t, "beta", res.Results[0].Variant)
}

func TestRepeats(t *testing.T) {
	res := runVmshed(t, vmshedOpts{
		VmsToml:   defaultVmsToml,
		TestsToml: defaultTestsToml,
		ExtraArgs: []string{"--repeats", "3"},
	})

	require.Len(t, res.Results, 3)
	for _, r := range res.Results {
		assert.Equal(t, "SUCCESS", r.Status)
		assert.Equal(t, "mytest", r.Name)
	}
	assert.Equal(t, 3, countSubcommand(res.VirterCalls, "vm exec"))
}

func TestMultipleVMCounts(t *testing.T) {
	res := runVmshed(t, vmshedOpts{
		VmsToml:   defaultVmsToml,
		TestsToml: multiVmcountTestsToml,
		ExtraArgs: []string{"--nvms", "2"},
	})

	require.Len(t, res.Results, 2)
	for _, r := range res.Results {
		assert.Equal(t, "SUCCESS", r.Status)
	}

	vmCounts := map[int]bool{}
	for _, r := range res.Results {
		vmCounts[r.VMCount] = true
	}
	assert.True(t, vmCounts[1], "expected a run with VM count 1")
	assert.True(t, vmCounts[2], "expected a run with VM count 2")
}

func TestVariants(t *testing.T) {
	res := runVmshed(t, vmshedOpts{
		VmsToml:   defaultVmsToml,
		TestsToml: variantsTestsToml,
	})

	require.Len(t, res.Results, 2)

	variants := map[string]bool{}
	for _, r := range res.Results {
		variants[r.Variant] = true
	}
	assert.True(t, variants["alpha"], "expected variant alpha")
	assert.True(t, variants["beta"], "expected variant beta")
}

func TestNeedAllPlatforms(t *testing.T) {
	res := runVmshed(t, vmshedOpts{
		VmsToml:   twoVmsToml,
		TestsToml: needAllPlatformsTestsToml,
		ExtraArgs: []string{"--repeats", "10"},
	})

	// Each repeat produces one run per base image, so 20 results total.
	require.Len(t, res.Results, 20)
	for _, r := range res.Results {
		assert.Equal(t, "SUCCESS", r.Status)
		require.Len(t, r.BaseImages, 1)
	}

	imageCount := map[string]int{}
	for _, r := range res.Results {
		imageCount[r.BaseImages[0]]++
	}
	assert.Equal(t, 10, imageCount["imageA"])
	assert.Equal(t, 10, imageCount["imageB"])
}

func TestSameVMs(t *testing.T) {
	res := runVmshed(t, vmshedOpts{
		VmsToml:   twoVmsToml,
		TestsToml: sameVmsTestsToml,
		ExtraArgs: []string{"--nvms", "2", "--repeats", "10"},
	})

	require.Len(t, res.Results, 10)
	for _, r := range res.Results {
		assert.Equal(t, 2, r.VMCount)
		require.Len(t, r.BaseImages, 2)
		assert.Equal(t, r.BaseImages[0], r.BaseImages[1],
			"samevms should use the same base image for both VMs")
	}
}

func TestVMTags(t *testing.T) {
	res := runVmshed(t, vmshedOpts{
		VmsToml:   taggedVmsToml,
		TestsToml: vmTagsTestsToml,
		ExtraArgs: []string{"--repeats", "10"},
	})

	require.Len(t, res.Results, 20)

	gpuResults := resultsByName(res.Results, "gputest")
	require.Len(t, gpuResults, 10)
	for _, r := range gpuResults {
		require.Len(t, r.BaseImages, 1)
		assert.Equal(t, "imageB", r.BaseImages[0],
			"gputest should only use imageB which has the gpu tag")
	}
}

func TestVMTagFlag(t *testing.T) {
	res := runVmshed(t, vmshedOpts{
		VmsToml:   taggedVmsToml,
		TestsToml: vmTagsTestsToml,
		ExtraArgs: []string{"--vm-tag", "gpu", "--repeats", "10"},
	})

	require.Len(t, res.Results, 20)
	for _, r := range res.Results {
		require.Len(t, r.BaseImages, 1)
		assert.Equal(t, "imageB", r.BaseImages[0],
			"--vm-tag gpu should leave only imageB for every test")
	}
}

func TestVMTagFlagAllTagsRequired(t *testing.T) {
	res := runVmshed(t, vmshedOpts{
		VmsToml:   taggedVmsToml,
		TestsToml: vmTagsTestsToml,
		ExtraArgs: []string{"--vm-tag", "fast", "--vm-tag", "gpu", "--torun", "anytest", "--repeats", "10"},
	})

	require.Len(t, res.Results, 10)
	for _, r := range res.Results {
		require.Len(t, r.BaseImages, 1)
		assert.Equal(t, "imageB", r.BaseImages[0],
			"only imageB has both the fast and the gpu tag")
	}
}

func TestNetworkAddFail(t *testing.T) {
	res := runVmshed(t, vmshedOpts{
		VmsToml:      defaultVmsToml,
		TestsToml:    defaultTestsToml,
		VirterFailOn: "network add",
		// vmshed should fail due to skipped test
		ExitCode: 1,
	})

	assert.Equal(t, 0, countSubcommand(res.VirterCalls, "vm exec"),
		"no tests should have started after access network add failed")
	assert.Empty(t, res.Results, "no results should be written for network-add-fail test")
}

func TestTimeoutSoftAllSkipped(t *testing.T) {
	res := runVmshed(t, vmshedOpts{
		VmsToml:   defaultVmsToml,
		TestsToml: twoTestsToml,
		ExtraArgs: []string{"--timeout-soft", "1ns"},
		ExitCode:  0,
	})

	assert.Equal(t, 0, countSubcommand(res.VirterCalls, "vm exec"),
		"no tests should have started after soft timeout")
	assert.Empty(t, res.Results, "no results should be written for timeout-soft-skipped test")
}

func TestTimeoutSoftPartialRun(t *testing.T) {
	res := runVmshed(t, vmshedOpts{
		VmsToml:       defaultVmsToml,
		TestsToml:     twoTestsToml,
		VirterDelayOn: "vm exec",
		VirterDelay:   "1s",
		ExtraArgs:     []string{"--timeout-soft", "200ms"},
		ExitCode:      0,
	})

	assert.Equal(t, 1, countSubcommand(res.VirterCalls, "vm exec"),
		"only one test should have started before soft timeout")
	require.Len(t, res.Results, 1)
	assert.Equal(t, "SUCCESS", res.Results[0].Status)
}

func TestRandomOrder(t *testing.T) {
	res := runVmshed(t, vmshedOpts{
		VmsToml:   defaultVmsToml,
		TestsToml: manyTestsToml,
	})

	require.Len(t, res.Results, 10)
	for _, r := range res.Results {
		assert.Equal(t, "SUCCESS", r.Status)
	}

	// Extract test names in execution order from vm exec calls.
	var execOrder []string
	for _, c := range res.VirterCalls {
		if name := c.TestName(); name != "" {
			execOrder = append(execOrder, name)
		}
	}

	require.Len(t, execOrder, 10)

	sorted := []string{"test_a", "test_b", "test_c", "test_d", "test_e", "test_f", "test_g", "test_h", "test_i", "test_j"}

	// With 10 tests, the chance of a random permutation matching sorted
	// order is 1/10! ≈ 1/3628800, so this is safe to assert.
	assert.NotEqual(t, sorted, execOrder,
		"test execution order should be randomized, not sorted")
}
