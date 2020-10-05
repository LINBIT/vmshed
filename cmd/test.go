package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

type test struct {
	VMCount          []int    `toml:"vms"`
	Tags             []string `toml:"tags"`
	SameVMs          bool     `toml:"samevms"`          // test need the same Distribution
	NeedAllPlatforms bool     `toml:"needallplatforms"` // test need to run on all platforms
	Variants         []string `toml:"variants"`         // only run on given variants, if empty all
}

type testRun struct {
	testName string
	testID   string
	outDir   string
	vms      []vm
	variant  variant
}

type TestResulter interface {
	ExecTime() time.Duration
	Err() error
}

// collect information about individual test runs
// the interface is similar to the log package (which it also uses)
type testResult struct {
	log         bytes.Buffer // log messages of the framework (starting test, timing information,...)
	testLog     bytes.Buffer // output of the test itself ('virter vm exec' output)
	execTime    time.Duration
	err         error
	timeout     bool
	stateString string
}

func (r testResult) ExecTime() time.Duration {
	return r.execTime
}

func (r testResult) Err() error {
	return r.err
}

func (r testResult) String() string {
	return r.stateString
}

func performTest(ctx context.Context, suiteRun *testSuiteRun, run *testRun, ids []int) (string, testResult) {
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
		var disks []string
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
		if len(v.Disks) > 0 {
			disks = v.Disks
		} else {
			disks = []string{"name=data,size=2G,bus=scsi"}
		}
		instance := vmInstance{
			ImageName: suiteRun.vmSpec.ImageName(&v),
			nr:        ids[i],
			memory:    memory,
			vcpus:     vcpus,
			disks:     disks,
		}
		vms = append(vms, instance)
	}

	testRes := execTest(ctx, suiteRun, run, vms...)

	testRes.stateString = "SUCCESS"
	var testErr error
	if ctx.Err() != nil {
		testRes.stateString = "CANCELED"
		testErr = fmt.Errorf("canceled")
	} else if testRes.timeout {
		testRes.stateString = "FAILED (TIMEOUT)"
		testErr = fmt.Errorf("timeout: %w", testRes.err)
	} else if testRes.err != nil {
		testRes.stateString = "FAILED"
		testErr = testRes.err
	}

	var report bytes.Buffer

	fmt.Fprintln(&report, "|===================================================================================================")
	fmt.Fprintf(&report, "| ** Results for %s - %s\n", run.testID, testRes.stateString)
	jobURL := os.Getenv("CI_JOB_URL")
	if jobURL != "" {
		fmt.Fprintf(&report, "| ** %s/artifacts/browse/%s\n", jobURL, run.outDir)
	}
	fmt.Fprintln(&report, "|===================================================================================================")
	logLines := strings.Split(strings.TrimSpace(testRes.log.String()), "\n")
	for _, line := range logLines {
		fmt.Fprintln(&report, "|", line)
	}

	testLog := testRes.testLog.Bytes()
	if suiteRun.jenkins.IsActive() {
		if err := ioutil.WriteFile(filepath.Join(run.outDir, "test.log"), testLog, 0644); err != nil {
			fmt.Fprintf(&report, "| FAILED to write log; suppressing original error: %v\n", testErr)
			testErr = err
		}

		if err := suiteRun.jenkins.XMLLog("test-results", run.testID, testRes, testLog); err != nil {
			fmt.Fprintf(&report, "| FAILED to write XML log; suppressing original error: %v\n", testErr)
			testErr = err
		}
	} else {
		fmt.Fprintf(&report, "| Test log for %s:\n", run.testID)
		testLogLines := strings.Split(strings.TrimSpace(string(testLog)), "\n")
		for _, line := range testLogLines {
			fmt.Fprintln(&report, "|", line)
		}
	}
	fmt.Fprintln(&report, "|===================================================================================================")

	return report.String(), testRes
}

func execTest(ctx context.Context, suiteRun *testSuiteRun, run *testRun, testnodes ...vmInstance) testResult {
	res := testResult{}
	logger := testLogger(&res.log, suiteRun.quiet)

	logger.Printf("EXECUTING: %s Nodes(%+v)", run.testID, testnodes)

	// Start VMs
	start := time.Now()
	err := startVMs(ctx, logger, run, testnodes...)
	defer shutdownVMs(logger, testnodes...)
	if err != nil {
		res.err = err
		return res
	}
	logger.Printf("EXECUTIONTIME: Starting VMs: %v", time.Since(start))

	testNameEnv := fmt.Sprintf("env.TEST_NAME=%s", run.testName)

	argv := []string{"virter", "vm", "exec",
		"--provision", suiteRun.testSpec.TestSuiteFile,
		"--set", testNameEnv}
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
	cmd.Env = virterEnv()
	cmd.Stderr = &res.testLog

	testCtx, cancel := context.WithTimeout(ctx, time.Duration(suiteRun.testSpec.TestTimeout))
	defer cancel()

	logger.Printf("EXECUTING TEST: %s", argv)
	start = time.Now()
	testErr := cmdRunTerm(testCtx, logger, cmd)
	logger.Printf("EXECUTIONTIME: Running test %s: %v", run.testID, time.Since(start))

	if exitErr, ok := testErr.(*exec.ExitError); ok {
		exitErr.Stderr = res.testLog.Bytes()
	}

	res.err = testErr
	res.timeout = testCtx.Err() != nil

	// copy artifacts from VMs
	if suiteRun.jenkins.IsActive() {
		for _, vm := range testnodes {
			for _, directory := range suiteRun.testSpec.Artifacts {
				// tgtPath will be /outdir/logs/{testname}/{vmname}/copy/path
				tgtPath := filepath.Join(run.outDir, vm.vmName(), filepath.Dir(directory))
				os.MkdirAll(tgtPath, 0755)
				if err := copyDir(logger, vm, directory, tgtPath); err != nil {
					logger.Printf("ARTIFACTCOPY: FAILED copy artifact directory %s: %s", directory, err.Error())
					dumpStderr(logger, err)
				}
			}
		}
	}

	return res
}

func copyDir(logger log.FieldLogger, vm vmInstance, srcDir string, hostDir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	args := []string{"virter", "vm", "cp", vm.vmName() + ":" + srcDir, hostDir}
	logger.Printf("EXECUTING VIRTER COPY: %s", args)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = virterEnv()
	return cmdStderrTerm(ctx, logger, cmd)
}

func testLogger(out io.Writer, quiet bool) *log.Logger {
	logger := log.New()
	logger.Out = out
	logger.Formatter = &log.TextFormatter{
		DisableQuote:    true,
		TimestampFormat: "15:04:05.000",
	}

	if !quiet {
		logger.AddHook(&StandardLoggerHook{})
	}

	return logger
}

// StandardLoggerHook duplicates log messages to the standard logger
type StandardLoggerHook struct {
}

func (hook *StandardLoggerHook) Fire(entry *log.Entry) error {
	logEntry := *entry
	logEntry.Logger = log.StandardLogger()
	logEntry.Log(logEntry.Level, logEntry.Message)
	return nil
}

func (hook *StandardLoggerHook) Levels() []log.Level {
	return []log.Level{
		log.PanicLevel,
		log.FatalLevel,
		log.ErrorLevel,
		log.WarnLevel,
		log.InfoLevel,
		log.DebugLevel,
	}
}
