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

	"github.com/rck/errorlog"
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

func execTests(suiteRun *testSuiteRun, nrPool chan int) (int, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var overallFailed int
	var testLogLock sync.Mutex // tests run in parallel, but we want the result blocks/logs somehow serialized.
	var testsWG sync.WaitGroup

	errs := errorlog.NewErrorLog()
	start := time.Now()
	for _, run := range suiteRun.testRuns {
		if suiteRun.failTest && errs.Len() > 0 {
			break
		}

		var vms []vmInstance
		for _, vm := range run.vms {
			nr := <-nrPool

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
				nr:        nr,
				memory:    memory,
				vcpus:     vcpus,
			}
			vms = append(vms, v)
		}

		testsWG.Add(1)
		go func(run testRun, testnodes ...vmInstance) {
			defer testsWG.Done()

			stTest := time.Now()
			testRes := execTest(ctx, suiteRun, run, nrPool, testnodes...)
			if err := testRes.err; err != nil {
				errs.Append(err)
				if suiteRun.failTest {
					log.Println(suiteRun.cmdName, "ERROR:", "Canceling ctx of running tests")
					cancel()
				}
			}
			testRes.execTime = time.Since(stTest)
			testRes.AppendLog(suiteRun.quiet, "EXECUTIONTIME: %s, %v", run.testID, testRes.execTime)

			testLogLock.Lock()
			defer testLogLock.Unlock()

			state := "SUCCESS"
			if testRes.err != nil {
				state = "FAILED"
			}
			fmt.Println("===========================================================================")
			fmt.Printf("| ** Results for %s - %s\n", run.testID, state)
			if suiteRun.jenkins.IsActive() {
				fmt.Printf("| ** %s/artifact/%s\n", os.Getenv("BUILD_URL"), run.testDirOut)
			}
			fmt.Println("===========================================================================")
			testLog := testRes.Log()
			fmt.Print(&testLog)

			if suiteRun.jenkins.IsActive() {
				testLog := testRes.TestLog()
				if err := suiteRun.jenkins.Log(run.testDirOut, "test.log", &testLog); err != nil {
					errs.Append(err)
				}

				xmllog := testRes.TestLog()
				if err := suiteRun.jenkins.XMLLog("test-results", run.testID, testRes, &xmllog); err != nil {
					errs.Append(err)
				}
			} else {
				testLog := testRes.TestLog()
				fmt.Printf("Test log for %s\n", run.testID)
				fmt.Print(&testLog)
			}
			fmt.Printf("END Results for %s\n", run.testID)
		}(run, vms...)

	}
	testsWG.Wait()
	log.Println(suiteRun.cmdName, "EXECUTIONTIME all tests:", time.Since(start))

	nErrs := errs.Len()
	if nErrs > 0 {
		overallFailed += nErrs
		log.Println("ERROR: Printing errors for all tests")
		for _, err := range errs.Errs() {
			log.Println(suiteRun.cmdName, err)
			if exitErr, ok := err.(*exec.ExitError); ok {
				log.Print(string(exitErr.Stderr))
			}
		}
	}
	return overallFailed, nil
}

func execTest(ctx context.Context, suiteRun *testSuiteRun, run testRun, nrPool chan<- int, testnodes ...vmInstance) *testResult {
	res := newTestResult(suiteRun.cmdName)

	// we always want to hand back the random VMS
	defer func() {
		for _, vm := range testnodes {
			nrPool <- vm.nr
		}
	}()

	// always also print the header
	res.AppendLog(false, "EXECUTING: %s Nodes(%+v)", run.testID, testnodes)

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
