package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/rck/errorlog"
	uuid "github.com/satori/go.uuid"
)

// TODO(rck): rm, this is nasty
var systemdScope sync.WaitGroup // VM starts (via systemd Add(1), and defer a go routine that waits. Use this as a signal that all VMs terminated after every group of tests

type testGroup struct {
	NrVMs            int      `json:"vms"`
	Tests            []string `json:"tests"`
	SameVMs          []string `json:"samevms"`          // tests that need the same Distribution
	NeedZFS          []string `json:"needzfs"`          // tests that need the zfs in their VM
	NeedPostgres     []string `json:"needpostgres"`     // tests that need postgres in their VM
	NeedMariaDB      []string `json:"needmariadb"`      // tests that need mariaDB in their VM
	NeedAllPlatforms []string `json:"needallplatforms"` // tests that need to run on all platforms
}

type testOption struct {
	needsSameVMs, needsZFS, needsPostgres, needsMariaDB, needsAllPlatforms bool
	platformIdx                                                            int
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

func execTests(tests []testGroup, nrVMs int, vmPool chan vmInstance) (int, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var overallFailed int
	var testLogLock sync.Mutex // tests run in parallel, but we want the result blocks/logs somehow serialized.
	var testsWG sync.WaitGroup
	for _, testGrp := range tests {
		if testGrp.NrVMs+1 > nrVMs { // +1 for the coordinator
			return 1, fmt.Errorf("This test group requires %d VMs (and a controller), but we only have %d VMs overall", testGrp.NrVMs, nrVMs)
		}

		allPlatforms := make(map[string]int)
		var allTests []string

		// multiplicate tests that need to run for all platforms
		for _, t := range testGrp.Tests {
			allTests = append(allTests, t)
			for _, a := range testGrp.NeedAllPlatforms {
				if a == t {
					for i := 0; i < len(allVMs)-1; i++ {
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
			for _, z := range testGrp.NeedZFS {
				if z == t {
					to.needsZFS = true
					break
				}
			}
			for _, m := range testGrp.NeedMariaDB {
				if m == t {
					to.needsMariaDB = true
					break
				}
			}
			for _, p := range testGrp.NeedPostgres {
				if p == t {
					to.needsPostgres = true
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

			controller := <-vmPool
			var vms []vmInstance
			for i := 0; i < testGrp.NrVMs; i++ {
				vms = append(vms, <-vmPool)
			}

			testsWG.Add(1)
			go func(st string, to testOption, controller vmInstance, testnodes ...vmInstance) {
				defer testsWG.Done()

				stTest := time.Now()
				testRes := execTest(ctx, st, to, vmPool, controller, testnodes...)
				if err := testRes.err; err != nil {
					errs.Append(err)
					if *failTest {
						log.Println(cmdName, "ERROR:", "Canceling ctx of running tests")
						cancel()
					}
				}
				testRes.execTime = time.Since(stTest)
				testOut := fmt.Sprintf("%s-%d-%d", st, testGrp.NrVMs, to.platformIdx)
				testDirOut := "log/" + testOut
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
			}(t, to, controller, vms...)

		}
		testsWG.Wait()
		systemdScope.Wait()
		log.Println(cmdName, "Group:", testGrp.NrVMs, "EXECUTIONTIME for Group:", testGrp.NrVMs, time.Since(start))

		nErrs := errs.Len()
		if nErrs > 0 {
			overallFailed += nErrs
			log.Println("ERROR: Printing erros for Group:", testGrp.NrVMs)
			for _, err := range errs.Errs() {
				log.Println(cmdName, err)
			}
			if *failGrp || *failTest {
				return overallFailed, errors.New("At least one test in the test group failed, giving up early")
			}
		}
	}
	return overallFailed, nil
}

func execTest(ctx context.Context, test string, to testOption, vmPool chan<- vmInstance, origController vmInstance, origTestnodes ...vmInstance) *testResult {
	res := newTestResult(cmdName)

	// we always want to hand back the random VMS
	defer func() {
		vmPool <- origController
		for _, vm := range origTestnodes {
			vmPool <- vm
		}
	}()
	// but we might need to actually start different ones
	controller, testnodes, err := finalVMs(to, origController, origTestnodes...)
	if err != nil {
		res.err = err
		return res
	}

	// set uuids
	controller.CurrentUUID = uuid.Must(uuid.NewV4()).String()
	for i := 0; i < len(testnodes); i++ {
		testnodes[i].CurrentUUID = uuid.Must(uuid.NewV4()).String()
	}

	// always also print the header
	testInstance := fmt.Sprintf("%s-%d-%d", test, len(testnodes), to.platformIdx)
	res.AppendLog(false, "EXECUTING: %s in %+v Nodes(%+v)", testInstance, controller, testnodes)

	// Start VMs
	start := time.Now()
	err = startVMs(test, res, to, controller, testnodes...)
	defer shutdownVMs(res, controller, testnodes...)
	if err != nil {
		res.err = err
		return res
	}
	res.AppendLog(*quiet, "EXECUTIONTIME: Starting VM scopes: %v", time.Since(start))

	// SSH ping the controller. If the controller is ready, we are ready to go
	// The controller itself then waits for the nodes under test (or gives up)
	res.AppendLog(*quiet, "EXECUTING: SSH-pinging for %s", testInstance)
	if err := sshPing(ctx, res, controller); err != nil {
		res.err = err
		return res
	}
	res.AppendLog(*quiet, "EXECUTIONTIME: SSH-pinging: %v", time.Since(start))

	var testvms []string
	for _, t := range testnodes {
		testvms = append(testvms, fmt.Sprintf("vm-%d", t.nr))
	}

	var ts string // test (shell) script
	switch *testSuite {
	case "drbd9", "drbdproxy":
		ts = "d9ts"
	case "linstor":
		ts = "linstorts"
	case "golinstor":
		ts = "golinstorts"
	default:
		ts = "doesnotexist"
	}

	envPrefix := "LB_TEST_"
	var env []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, envPrefix) {
			env = append(env, strings.TrimPrefix(e, envPrefix))
		}
	}

	payload := fmt.Sprintf("%s:leader:tests=%s:undertest=%s:env='%s'",
		*testSuite, test, strings.Join(testvms, ","), strings.Join(env, ","))
	argv := []string{"ssh", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
		fmt.Sprintf("root@vm-%d", controller.nr), "/payloads/" + ts, payload}

	res.AppendLog(*quiet, "EXECUTING the actual test via ssh: %s", argv)

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
