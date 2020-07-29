package cmd

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"
)

type testGroup struct {
	NrVMs            int      `toml:"vms"`
	Tests            []string `toml:"tests"`
	SameVMs          []string `toml:"samevms"`          // tests that need the same Distribution
	NeedAllPlatforms []string `toml:"needallplatforms"` // tests that need to run on all platforms
}

type testRun struct {
	testName   string
	testID     string
	testDirOut string
	consoleDir string
	vms        []vm
}

type TestResulter interface {
	ExecTime() time.Duration
	Err() error
}

// collect information about individual test runs
// the interface is similar to the log package (which it also uses)
// TODO(rck): we have one per test, and the mutex only protects the overall log, but not access to the buffer, this would require getters/extra pkg.
type testResult struct {
	log       bytes.Buffer // log messages of the framework (starting test, timing information,...)
	logLogger *log.Logger

	testLog    bytes.Buffer // output of the test itself ('virter vm exec' output)
	testLogger *log.Logger

	execTime time.Duration

	err error
	sync.Mutex
}

func newTestResult(prefix string) *testResult {
	tr := testResult{}
	p := prefix + ": "
	tr.logLogger = log.New(&tr.log, p, log.Ldate)
	tr.testLogger = log.New(&tr.testLog, p, log.Ldate)
	return &tr
}

func (r *testResult) ExecTime() time.Duration {
	return r.execTime
}

func (r *testResult) Err() error {
	return r.err
}

func (r *testResult) Log() bytes.Buffer {
	r.Lock()
	defer r.Unlock()
	return r.log
}

func (r *testResult) TestLog() bytes.Buffer {
	r.Lock()
	defer r.Unlock()
	return r.testLog
}

func (r *testResult) writeLog(logger *log.Logger, quiet bool, format string, v ...interface{}) {
	r.Lock()
	logger.Printf(format, v...)
	r.Unlock()

	// TODO(rck): this generates slightly different time stamps..
	if !quiet {
		log.Printf(format, v...)
	}
}

func (r *testResult) AppendLog(quiet bool, format string, v ...interface{}) {
	r.writeLog(r.logLogger, quiet, format, v...)
}

func (r *testResult) AppendTestLog(quiet bool, format string, v ...interface{}) {
	r.writeLog(r.testLogger, quiet, format, v...)
}

func performTest(ctx context.Context, suiteRun *testSuiteRun, run testRun, ids []int) (string, error) {
	var resultLog bytes.Buffer
	logger := log.New(&resultLog, "", 0)

	var vms []vmInstance
	for i, vm := range run.vms {
		var memory string
		var vcpus uint
		if vm.Memory != "" {
			memory = vm.Memory
		} else {
			memory = "4G"
		}
		if vm.VCPUs != 0 {
			vcpus = vm.VCPUs
		} else {
			vcpus = 4
		}
		v := vmInstance{
			ImageName: suiteRun.vmSpec.ImageName(vm),
			nr:        ids[i],
			memory:    memory,
			vcpus:     vcpus,
		}
		vms = append(vms, v)
	}

	stTest := time.Now()
	testRes := execTest(ctx, suiteRun, run, vms...)
	testErr := testRes.err
	testRes.execTime = time.Since(stTest)
	testRes.AppendLog(suiteRun.quiet, "EXECUTIONTIME: %s, %v", run.testID, testRes.execTime)

	state := "SUCCESS"
	if testRes.err != nil {
		state = "FAILED"
	}
	logger.Println("===========================================================================")
	logger.Printf("| ** Results for %s - %s\n", run.testID, state)
	if suiteRun.jenkins.IsActive() {
		logger.Printf("| ** %s/artifact/%s\n", os.Getenv("BUILD_URL"), run.testDirOut)
	}
	logger.Println("===========================================================================")
	testLog := testRes.Log()
	logger.Print(&testLog)

	if suiteRun.jenkins.IsActive() {
		testLog := testRes.TestLog()
		if err := suiteRun.jenkins.Log(run.testDirOut, "test.log", &testLog); err != nil {
			logger.Printf("FAILED to write log; suppressing original error: %v", testErr)
			testErr = err
		}

		xmllog := testRes.TestLog()
		if err := suiteRun.jenkins.XMLLog("test-results", run.testID, testRes, &xmllog); err != nil {
			logger.Printf("FAILED to write XML log; suppressing original error: %v", testErr)
			testErr = err
		}
	} else {
		testLog := testRes.TestLog()
		logger.Printf("Test log for %s\n", run.testID)
		logger.Print(&testLog)
	}
	logger.Printf("END Results for %s\n", run.testID)

	return resultLog.String(), testErr
}

func execTest(ctx context.Context, suiteRun *testSuiteRun, run testRun, testnodes ...vmInstance) *testResult {
	res := newTestResult(suiteRun.cmdName)

	res.AppendLog(suiteRun.quiet, "EXECUTING: %s Nodes(%+v)", run.testID, testnodes)

	// Start VMs
	start := time.Now()
	err := startVMs(res, run, suiteRun.quiet, testnodes...)
	defer shutdownVMs(res, suiteRun.quiet, testnodes...)
	if err != nil {
		res.err = err
		return res
	}
	res.AppendLog(suiteRun.quiet, "EXECUTIONTIME: Starting VMs: %v", time.Since(start))

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

	res.AppendLog(suiteRun.quiet, "EXECUTING the actual test: %s", argv)

	start = time.Now()

	ctx, cancel := context.WithTimeout(ctx, suiteRun.testTimeout)
	defer cancel()

	cmd := exec.Command(argv[0], argv[1:]...)

	testDone := make(chan struct{})
	go handleTestTermination(ctx, cmd, testDone, res, suiteRun.quiet)

	out, testErr := cmd.CombinedOutput()

	close(testDone)

	res.AppendTestLog(true, "%s\n", out)

	res.AppendLog(suiteRun.quiet, "EXECUTIONTIME: %s %v", run.testID, time.Since(start))
	if testErr != nil { // "real" error or ctx canceled
		res.err = fmt.Errorf("ERROR: %s %v", run.testID, testErr)
		if ctxErr := ctx.Err(); ctxErr != nil {
			res.err = fmt.Errorf("%v %v", res.err, ctxErr)
		}
		return res
	}
	res.AppendLog(suiteRun.quiet, "SUCCESS: %s", run.testID)

	return res
}

func handleTestTermination(ctx context.Context, cmd *exec.Cmd, done <-chan struct{}, res *testResult, quiet bool) {
	select {
	case <-ctx.Done():
		res.AppendLog(quiet, "TERMINATING test with SIGINT")
		cmd.Process.Signal(os.Interrupt)
		select {
		case <-time.After(10 * time.Second):
			res.AppendLog(quiet, "WARNING! TERMINATING test with SIGKILL")
			cmd.Process.Kill()
		case <-done:
		}
	case <-done:
	}
}
