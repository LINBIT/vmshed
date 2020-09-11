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

type testGroup struct {
	NrVMs            int      `toml:"vms"`
	Tests            []string `toml:"tests"`
	SameVMs          []string `toml:"samevms"`          // tests that need the same Distribution
	NeedAllPlatforms []string `toml:"needallplatforms"` // tests that need to run on all platforms
}

type testRun struct {
	testName string
	testID   string
	outDir   string
	vms      []vm
}

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
}

func (r *testResult) ExecTime() time.Duration {
	return r.execTime
}

func (r *testResult) Err() error {
	return r.err
}

func performTest(ctx context.Context, suiteRun *testSuiteRun, run *testRun, ids []int) (string, error) {
	if run.outDir != "" {
		err := os.MkdirAll(run.outDir, 0755)
		if err != nil {
			return "", err
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
	testErr := testRes.err

	var report bytes.Buffer

	fmt.Fprintln(&report, "|===================================================================================================")
	fmt.Fprintf(&report, "| ** Results for %s - %s\n", run.testID, testStateString(testRes.err))
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

	return report.String(), testErr
}

func testStateString(err error) string {
	if err != nil {
		return "FAILED"
	}
	return "SUCCESS"
}

func execTest(ctx context.Context, suiteRun *testSuiteRun, run *testRun, testnodes ...vmInstance) *testResult {
	res := testResult{}
	logger := testLogger(&res.log, suiteRun.quiet)

	logger.Printf("EXECUTING: %s Nodes(%+v)", run.testID, testnodes)

	// Start VMs
	start := time.Now()
	err := startVMs(logger, run, testnodes...)
	defer shutdownVMs(logger, testnodes...)
	if err != nil {
		res.err = err
		return &res
	}
	logger.Printf("EXECUTIONTIME: Starting VMs: %v", time.Since(start))

	testNameEnv := fmt.Sprintf("env.TEST_NAME=%s", run.testName)

	argv := []string{"virter", "vm", "exec",
		"--provision", suiteRun.testSpec.TestSuiteFile,
		"--set", testNameEnv}
	for _, override := range suiteRun.overrides {
		argv = append(argv, "--set", override)
	}
	for _, vm := range testnodes {
		argv = append(argv, vm.vmName())
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stderr = &res.testLog

	ctx, cancel := context.WithTimeout(ctx, time.Duration(suiteRun.testSpec.TestTimeout))
	defer cancel()

	logger.Printf("EXECUTING TEST: %s", argv)
	start = time.Now()
	testErr := cmdRunTerm(ctx, logger, cmd)
	logger.Printf("EXECUTIONTIME: Running test %s: %v", run.testID, time.Since(start))

	if exitErr, ok := testErr.(*exec.ExitError); ok {
		exitErr.Stderr = res.testLog.Bytes()
	}

	if testErr != nil { // "real" error or ctx canceled
		if ctxErr := ctx.Err(); ctxErr != nil {
			res.err = fmt.Errorf("%s: (context error: %v): %w", run.testID, ctxErr, testErr)
		} else {
			res.err = fmt.Errorf("%s: %w", run.testID, testErr)
		}
	}

	return &res
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
