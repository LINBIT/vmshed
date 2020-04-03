package cmd

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
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

type testOption struct {
	needsSameVMs      bool
	needsAllPlatforms bool
	platformIdx       int
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

	inVM       bytes.Buffer // combined output of stdout/stderr of the test itself (ssh-output)
	inVMLogger *log.Logger

	execTime time.Duration

	err error
	sync.Mutex
}

func newTestResult(prefix string) *testResult {
	tr := testResult{}
	p := prefix + ": "
	tr.logLogger = log.New(&tr.log, p, log.Ldate)
	tr.inVMLogger = log.New(&tr.inVM, p, log.Ldate)
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

func (r *testResult) InVM() bytes.Buffer {
	r.Lock()
	defer r.Unlock()
	return r.inVM
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

func (r *testResult) AppendInVM(quiet bool, format string, v ...interface{}) {
	r.writeLog(r.inVMLogger, quiet, format, v...)
}

func execTests(testSpec *testSpecification, nrVMs int, nrPool chan int) (int, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var overallFailed int
	var testLogLock sync.Mutex // tests run in parallel, but we want the result blocks/logs somehow serialized.
	var testsWG sync.WaitGroup
	for _, testGrp := range testSpec.TestGroups {
		if testGrp.NrVMs > nrVMs {
			return 1, fmt.Errorf("This test group requires %d VMs, but we only have %d VMs overall", testGrp.NrVMs, nrVMs)
		}

		allPlatforms := make(map[string]int)
		var allTests []string

		// multiplicate tests that need to run for all platforms
		for _, t := range testGrp.Tests {
			allTests = append(allTests, t)
			for _, a := range testGrp.NeedAllPlatforms {
				if a == t {
					for i := 0; i < len(vmSpec.VMs)-1; i++ {
						allTests = append(allTests, t)
					}
					break
				}
			}
		}

		errs := errorlog.NewErrorLog()
		start := time.Now()
		for _, t := range allTests {
			if *failTest && errs.Len() > 0 {
				break
			}

			var to testOption
			for _, s := range testGrp.SameVMs {
				if s == t {
					to.needsSameVMs = true
					break
				}
			}
			for _, a := range testGrp.NeedAllPlatforms {
				if a == t {
					to.needsAllPlatforms = true
					break
				}
			}

			to.platformIdx = allPlatforms[t]
			allPlatforms[t]++

			var vms []vmInstance
			for i := 0; i < testGrp.NrVMs; i++ {
				nr := <-nrPool
				r, err := rand.Int(rand.Reader, big.NewInt(int64(len(vmSpec.VMs))))
				if err != nil {
					return 1, err
				}
				v := vmInstance{
					ImageName: vmSpec.ImageName(vmSpec.VMs[r.Int64()]),
					nr:        nr,
				}
				vms = append(vms, v)
			}
			vms, err := finalVMs(to, vms...)
			if err != nil {
				return 1, err
			}

			testsWG.Add(1)
			go func(st string, to testOption, testnodes ...vmInstance) {
				defer testsWG.Done()

				stTest := time.Now()
				testRes := execTest(ctx, testSpec, st, to, nrPool, testnodes...)
				if err := testRes.err; err != nil {
					errs.Append(err)
					if *failTest {
						log.Println(cmdName, "ERROR:", "Canceling ctx of running tests")
						cancel()
					}
				}
				testRes.execTime = time.Since(stTest)
				testOut := testIdString(st, testGrp.NrVMs, to.platformIdx)
				testDirOut := filepath.Join("log", testOut)
				testRes.AppendLog(*quiet, "EXECUTIONTIME: %s, %v", testOut, testRes.execTime)

				testLogLock.Lock()
				defer testLogLock.Unlock()

				state := "SUCCESS"
				if testRes.err != nil {
					state = "FAILED"
				}
				fmt.Println("===========================================================================")
				fmt.Printf("| ** Results for %s - %s\n", testOut, state)
				if jenkins.IsActive() {
					fmt.Printf("| ** %s/artifact/%s\n", os.Getenv("BUILD_URL"), testDirOut)
				}
				fmt.Println("===========================================================================")
				testLog := testRes.Log()
				fmt.Print(&testLog)

				if jenkins.IsActive() {
					inVM := testRes.InVM()
					if err := jenkins.Log(testDirOut, "inVM.log", &inVM); err != nil {
						errs.Append(err)
					}

					xmllog := testRes.InVM()
					if err := jenkins.XMLLog("test-results", testOut, testRes, &xmllog); err != nil {
						errs.Append(err)
					}
				} else {
					inVM := testRes.InVM()
					fmt.Printf("In VM/Test log for %s\n", testOut)
					fmt.Print(&inVM)
				}
				fmt.Printf("END Results for %s\n", testOut)
			}(t, to, vms...)

		}
		testsWG.Wait()
		log.Println(cmdName, "Group:", testGrp.NrVMs, "EXECUTIONTIME for Group:", testGrp.NrVMs, time.Since(start))

		nErrs := errs.Len()
		if nErrs > 0 {
			overallFailed += nErrs
			log.Println("ERROR: Printing erros for Group:", testGrp.NrVMs)
			for _, err := range errs.Errs() {
				log.Println(cmdName, err)
				if exitErr, ok := err.(*exec.ExitError); ok {
					log.Print(string(exitErr.Stderr))
				}
			}
			if *failGrp || *failTest {
				return overallFailed, errors.New("At least one test in the test group failed, giving up early")
			}
		}
	}
	return overallFailed, nil
}

func execTest(ctx context.Context, testSpec *testSpecification, test string, to testOption, nrPool chan<- int, testnodes ...vmInstance) *testResult {
	res := newTestResult(cmdName)

	// we always want to hand back the random VMS
	defer func() {
		for _, vm := range testnodes {
			nrPool <- vm.nr
		}
	}()

	// always also print the header
	testInstance := fmt.Sprintf("%s-%d-%d", test, len(testnodes), to.platformIdx)
	res.AppendLog(false, "EXECUTING: %s Nodes(%+v)", testInstance, testnodes)

	// Start VMs
	start := time.Now()
	err := startVMs(res, to, testnodes...)
	defer shutdownVMs(res, testnodes...)
	if err != nil {
		res.err = err
		return res
	}
	res.AppendLog(*quiet, "EXECUTIONTIME: Starting VMs: %v", time.Since(start))

	argv := []string{"virter", "vm", "exec",
		"--provision", testSpec.TestSuiteFile}
	for _, vm := range testnodes {
		argv = append(argv, vm.vmName())
	}

	res.AppendLog(*quiet, "EXECUTING the actual test: %s", argv)

	start = time.Now()
	ctx, cancel := context.WithTimeout(ctx, *testTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out, testErr := cmd.CombinedOutput()
	res.AppendInVM(true, "%s\n", out)
	res.AppendLog(*quiet, "EXECUTIONTIME: %s %v", testInstance, time.Since(start))
	if testErr != nil { // "real" error or ctx canceled
		res.err = fmt.Errorf("ERROR: %s %v", testInstance, testErr)
		if ctxErr := ctx.Err(); ctxErr != nil {
			res.err = fmt.Errorf("%v %v", res.err, ctxErr)
		}
		return res
	}
	res.AppendLog(*quiet, "SUCCESS: %s", testInstance)

	return res
}