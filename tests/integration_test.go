package tests

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

type vmshedOpts struct {
	VmsToml      []byte
	TestsToml    []byte
	VirterFailOn string
	ExtraArgs    []string
	ExitCode     int
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

	// Create a bin directory with mock virter symlinked as "virter"
	binDir := filepath.Join(dir, "bin")
	require.NoError(t, os.Mkdir(binDir, 0755))
	require.NoError(t, os.Symlink(mockVirterBinary(t), filepath.Join(binDir, "virter")))

	outDir := filepath.Join(dir, "out")

	args := []string{
		"--vms", vmsToml,
		"--tests", testsToml,
		"--out-dir", outDir,
		"--nvms", "1",
		"--startvm", "2",
		"--seed", "1",
	}
	args = append(args, opts.ExtraArgs...)

	virterLog := filepath.Join(dir, "virter.log")

	cmd := exec.Command(vmshedBinary(t), args...)
	cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))
	cmd.Env = append(cmd.Env, "MOCK_VIRTER_LOG="+virterLog)
	if opts.VirterFailOn != "" {
		cmd.Env = append(cmd.Env, "MOCK_VIRTER_FAIL_ON="+opts.VirterFailOn)
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
