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

type testGroup struct {
	VMs     int      `json:"vms"`
	Tests   []string `json:"tests"`
	SameVMs []string `json:"samevms"` // tests that need the same Distribution
	NeedZFS []string `json:"needzfs"` // tests that need the zfs in their VM
}

type testOption struct {
	needsSameVMs, needsZFS bool
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
	var testLogLock sync.Mutex
	for _, testGrp := range tests {
		if testGrp.VMs+1 > nrVMs { // +1 for the coordinator
			return 1, fmt.Errorf("This test group requires %d VMs (and a controller), but we only have %d VMs overall", testGrp.VMs, nrVMs)
		}

		errs := errorlog.NewErrorLog()
		var testGrpWG sync.WaitGroup
		start := time.Now()
		for _, t := range testGrp.Tests {
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

			controller := <-vmPool
			var vms []vmInstance
			for i := 0; i < testGrp.VMs; i++ {
				vms = append(vms, <-vmPool)
			}

			if *failTest && errs.Len() > 0 {
				break
			}

			testGrpWG.Add(1)
			go func(st string, to testOption, controler vmInstance, testnodes ...vmInstance) {
				defer testGrpWG.Done()

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
				testOut := fmt.Sprintf("%s-%d", st, testGrp.VMs)
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
				if isJenkins() {
					fmt.Printf("| ** %s/artifact/%s\n", os.Getenv("BUILD_URL"), testDirOut)
				}
				fmt.Println("===========================================================================")
				testLog := testRes.Log()
				fmt.Print(&testLog)

				inVM := testRes.InVM()
				if isJenkins() {
					if err := jenkinsLog(testDirOut, "inVM.log", &inVM); err != nil {
						errs.Append(err)
					}

					if err := jenkinsXMLLog("test-results", testOut, testRes, &inVM); err != nil {
						errs.Append(err)
					}
				} else {
					fmt.Printf("In VM/Test log for %s\n", testOut)
					fmt.Print(&inVM)
				}
				fmt.Printf("END Results for %s\n", testOut)
			}(t, to, controller, vms...)

		}
		testGrpWG.Wait()
		log.Println(cmdName, "Group:", testGrp.VMs, "EXECUTIONTIME for Group:", testGrp.VMs, time.Since(start))

		nErrs := errs.Len()
		if nErrs > 0 {
			overallFailed += nErrs
			log.Println("ERROR: Printing erros for Group:", testGrp.VMs)
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
	testInstance := fmt.Sprintf("%s-%d", test, len(testnodes))
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

	var ts string
	switch *testSuite {
	case "drbd9":
		ts = "d9ts"
	case "linstor":
		ts = "linstorts"
	default:
		ts = "doesnotexist"
	}
	payload := fmt.Sprintf("%s:leader:tests=%s:undertest=%s",
		ts, test, strings.Join(testvms, ","))
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
