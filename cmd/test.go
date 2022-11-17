package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

type test struct {
	VMCount          []int       `toml:"vms"`
	VMTags           []string    `toml:"vm_tags"`
	SameVMs          bool        `toml:"samevms"`          // test need the same Distribution
	NeedAllPlatforms bool        `toml:"needallplatforms"` // test need to run on all platforms
	Variants         []string    `toml:"variants"`         // only run on given variants, if empty all
	Networks         []virterNet `toml:"networks"`         // Extra NIC to add to the VMs
}

type testRun struct {
	testName string
	testID   string
	outDir   string
	vms      []vm
	networks []virterNet
	variant  variant
}

type TestStatus string

const (
	StatusSkipped       TestStatus = "SKIPPED"
	StatusSuccess       TestStatus = "SUCCESS"
	StatusCanceled      TestStatus = "CANCELED"
	StatusFailedTimeout TestStatus = "FAILED(TO)"
	StatusFailed        TestStatus = "FAILED"
	StatusError         TestStatus = "ERROR" // Error running test
)

type TestResulter interface {
	ExecTime() time.Duration
	Err() error
}

// collect information about individual test runs
// the interface is similar to the log package (which it also uses)
type testResult struct {
	log      bytes.Buffer // log messages of the framework (starting test, timing information,...)
	testLog  bytes.Buffer // output of the test itself ('virter vm exec' output)
	execTime time.Duration
	err      error
	status   TestStatus
}

func (r testResult) ExecTime() time.Duration {
	return r.execTime
}

func (r testResult) Err() error {
	return r.err
}

func (r testResult) String() string {
	return string(r.status)
}

func performTest(ctx context.Context, suiteRun *testSuiteRun, run *testRun, ids []int, networkNames []string) (string, testResult) {
	if run.outDir != "" {
		err := os.MkdirAll(run.outDir, 0755)
		if err != nil {
			return "", testResult{err: err}
		}
	}

	var vms []vmInstance
	for i, v := range run.vms {
		var memory string
		var vcpus uint
		var bootCap string
		var disks []string
		var userName string

		if v.Memory != "" {
			memory = v.Memory
		} else {
			memory = "4G"
		}
		if v.VCPUs != 0 {
			vcpus = v.VCPUs
		} else {
			vcpus = 4
		}
		if v.BootCap != "" {
			bootCap = v.BootCap
		} else {
			bootCap = "10G"
		}
		if len(v.Disks) > 0 {
			disks = v.Disks
		} else {
			disks = []string{"name=data,size=2G,bus=scsi"}
		}
		userName = v.UserName

		instance := vmInstance{
			ImageName:    suiteRun.vmSpec.ImageName(&v),
			nr:           ids[i],
			memory:       memory,
			vcpus:        vcpus,
			bootCap:      bootCap,
			disks:        disks,
			networkNames: networkNames,
			UserName:     userName,
		}
		vms = append(vms, instance)
	}

	testRes := execTest(ctx, suiteRun, run, networkNames[0], vms)

	var report bytes.Buffer

	fmt.Fprintln(&report, "|===================================================================================================")
	fmt.Fprintf(&report, "| ** Results for %s - %s\n", run.testID, testRes.status)
	artifactsUrl := getArtifactsUrl(run.outDir)
	if artifactsUrl != "" {
		fmt.Fprintf(&report, "| ** %s\n", artifactsUrl)
	}

	fmt.Fprintln(&report, "|===================================================================================================")
	logLines := strings.Split(strings.TrimSpace(testRes.log.String()), "\n")
	for _, line := range logLines {
		fmt.Fprintln(&report, "|", line)
	}

	testLog := testRes.testLog.Bytes()
	if err := ioutil.WriteFile(filepath.Join(run.outDir, "test.log"), testLog, 0644); err != nil {
		fmt.Fprintf(&report, "| FAILED to write log; suppressing original error: %v\n", testRes.err)
		testRes.err = err
	}

	resultsDir := filepath.Join(suiteRun.outDir, "test-results")
	if err := XMLLog(resultsDir, run.testID, testRes, testLog); err != nil {
		fmt.Fprintf(&report, "| FAILED to write XML log; suppressing original error: %v\n", testRes.err)
		testRes.err = err
	}
	fmt.Fprintln(&report, "|===================================================================================================")

	if err := ioutil.WriteFile(filepath.Join(run.outDir, "report.log"), report.Bytes(), 0644); err != nil {
		log.Errorf("Failed to write report; suppressing original error: %v\n", testRes.err)
		testRes.err = err
	}

	return report.String(), testRes
}

func execTest(ctx context.Context, suiteRun *testSuiteRun, run *testRun, accessNetwork string, testnodes []vmInstance) testResult {
	res := testResult{}
	logger := TestLogger(run.testID, &res.log)

	logger.Debugf("EXECUTING: %s Nodes(%+v)", run.testID, testnodes)

	// Start VMs
	start := time.Now()
	err := startVMs(ctx, logger, run, testnodes...)
	defer shutdownVMs(logger, run.outDir, testnodes...)
	if err != nil {
		res.status = StatusError
		res.err = fmt.Errorf("failed to start VMs: %w", err)
		return res
	}
	logger.Debugf("EXECUTIONTIME: Starting VMs: %v", time.Since(start))

	testNameEnv := fmt.Sprintf("env.TEST_NAME=%s", run.testName)
	outDirValue := fmt.Sprintf("values.OutDir=%s", run.outDir)

	argv := []string{"virter"}
	if suiteRun.logFormatVirter != "" {
		argv = append(argv, "--logformat", suiteRun.logFormatVirter)
	}
	argv = append(argv,
		"vm", "exec",
		"--provision", suiteRun.testSpec.TestSuiteFile,
		"--set", testNameEnv,
		"--set", outDirValue)
	for _, override := range suiteRun.overrides {
		argv = append(argv, "--set", override)
	}
	// variant variables
	for key, value := range run.variant.Variables {
		argv = append(argv, "--set", "values."+key+"="+value)
	}
	for _, vm := range testnodes {
		argv = append(argv, vm.vmName())
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = virterEnv(accessNetwork)
	cmd.Stderr = &res.testLog

	testCtx, cancel := context.WithTimeout(ctx, time.Duration(suiteRun.testSpec.TestTimeout))
	defer cancel()

	logger.Debugf("EXECUTING TEST: %s", argv)
	start = time.Now()
	res.err = cmdRunTerm(testCtx, logger, cmd)
	timeout := testCtx.Err() != nil
	res.execTime = time.Since(start)
	logger.Debugf("EXECUTIONTIME: Running test %s: %v", run.testID, res.execTime)

	if exitErr, ok := res.err.(*exec.ExitError); ok {
		exitErr.Stderr = res.testLog.Bytes()
	}

	// copy artifacts from VMs
	for _, vm := range testnodes {
		for _, directory := range suiteRun.testSpec.Artifacts {
			// tgtPath will be /outdir/logs/{testname}/{vmname}/copy/path
			tgtPath := filepath.Join(run.outDir, vm.vmName(), filepath.Dir(directory))
			os.MkdirAll(tgtPath, 0755)
			if err := copyDir(logger, vm, run.outDir, directory, tgtPath); err != nil {
				logger.Infof("ARTIFACTCOPY: FAILED copy artifact directory %s: %s", directory, err.Error())
				dumpStderr(logger, err)
			}
		}
	}

	if ctx.Err() != nil {
		res.status = StatusCanceled
		res.err = fmt.Errorf("canceled")
	} else if timeout {
		res.status = StatusFailedTimeout
		res.err = fmt.Errorf("timeout: %w", res.err)
	} else if res.err != nil {
		res.status = StatusFailed
	} else {
		res.status = StatusSuccess
	}

	return res
}

func copyDir(logger log.FieldLogger, vm vmInstance, logDir string, srcDir string, hostDir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	args := []string{"virter", "vm", "cp", vm.vmName() + ":" + srcDir, hostDir}
	stderrPath := filepath.Join(logDir, fmt.Sprintf("vm_cp_%s_%s.log", vm.vmName(), strings.ReplaceAll(strings.TrimLeft(srcDir, "/"), "/", "-")))
	logger.Debugf("EXECUTING VIRTER COPY: %s", args)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = virterEnv(vm.networkNames[0])
	return cmdStderrTerm(ctx, logger, stderrPath, cmd)
}

func getArtifactsUrl(outdir string) string {
	jobURL := os.Getenv("CI_JOB_URL")
	if jobURL == "" {
		return ""
	}

	buildRoot := os.Getenv("CI_PROJECT_DIR")
	if buildRoot == "" {
		return ""
	}

	abs, err := filepath.Abs(outdir)
	if err != nil {
		return ""
	}

	relOut, err := filepath.Rel(buildRoot, abs)
	if err != nil {
		return ""
	}

	return fmt.Sprintf("%s/artifacts/browse/%s", jobURL, relOut)
}
