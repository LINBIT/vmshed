package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nightlyone/lockfile"
	"github.com/rck/errorlog"
)

type vm struct {
	Distribution string `"json:distribution"`
	Kernel       string `"json:kernel"`
}

type vmInstance struct {
	nr int
	vm
}

type test struct {
	VMs     int      `"json:vms"`
	Tests   []string `"json:tests"`
	SameVMs []string `"json:samevms"`
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

var systemdScope sync.WaitGroup

var (
	cmdName = filepath.Base(os.Args[0])

	vmSpec      = flag.String("vms", "vms.json", "File containing VM specification")
	testSpec    = flag.String("tests", "tests.json", "File containing test specification")
	startVM     = flag.Int("startvm", 1, "Number of the first VM to start in parallel")
	nrVMs       = flag.Int("nvms", 12, "Maximum number of VMs to start in parallel, starting at -startvm")
	failTest    = flag.Bool("failtest", false, "Stop executing tests when the first one failed")
	failGrp     = flag.Bool("failgroup", false, "Stop executing tests when at least one failed in the test group")
	quiet       = flag.Bool("quiet", false, "Don't print progess messages while tests are running")
	jenkins     = flag.String("jenkins", "", "If this is set to a path for the current job, text output is saved to files, logs get copied,...")
	sshTimeout  = flag.Duration("sshping", 3*time.Minute, "Timeout for ssh pinging the controller node")
	testTimeout = flag.Duration("testtime", 5*time.Minute, "Timeout for a single test execution in a VM")
)

func main() {
	flag.Parse()

	if *startVM <= 0 {
		log.Fatal(cmdName, "-startvm has to be positive")
	}
	if *nrVMs <= 0 {
		log.Fatal(cmdName, "-nvms has to be positive")
	}

	if isJenkins() {
		if !filepath.IsAbs(*jenkins) {
			log.Fatal(cmdName, *jenkins, "is not an absolute path")
		}
		if st, err := os.Stat(*jenkins); err != nil {
			log.Fatal(cmdName, "Could not stat ", *jenkins, err)
		} else if !st.IsDir() {
			log.Fatal(cmdName, *jenkins, "is not a directory")
		}
	}

	vmFile, err := os.Open(*vmSpec)
	if err != nil {
		log.Fatal(err)
	}

	dec := json.NewDecoder(vmFile)

	var vms []vm
	for {
		var vm vm
		if err := dec.Decode(&vm); err == io.EOF {
			break
		} else if err != nil {
			log.Fatal(err)
		}
		vms = append(vms, vm)
	}

	testFile, err := os.Open(*testSpec)
	if err != nil {
		log.Fatal(err)
	}

	dec = json.NewDecoder(testFile)

	var tests []test
	for {
		var test test
		if err := dec.Decode(&test); err == io.EOF {
			break
		} else if err != nil {
			log.Fatal(err)
		}
		tests = append(tests, test)
	}

	// generate random VMs
	vmPool := make(chan vmInstance, *nrVMs)
	for i := 0; i < *nrVMs; i++ {
		var vm vmInstance
		vm.nr = i + *startVM
		rndVM := vms[rand.Intn(len(vms))]
		vm.Distribution = rndVM.Distribution
		vm.Kernel = rndVM.Kernel

		vmPool <- vm

		lockName := fmt.Sprintf("%s.vm-%d.lock", cmdName, vm.nr)
		lock, err := lockfile.New(filepath.Join(os.TempDir(), lockName))
		if err != nil {
			log.Fatalf("Cannot init lock. reason: %v", err)
		}
		if err = lock.TryLock(); err != nil {
			log.Fatalf("Cannot lock %q, reason: %v", lock, err)
		}
		defer lock.Unlock()
	}

	start := time.Now()
	nFailed, err := execTests(tests, *nrVMs, vmPool)
	if err != nil {
		log.Println(cmdName, "ERROR:", err)
	}
	log.Println(cmdName, "OVERALL EXECUTIONTIME:", time.Since(start))

	systemdScope.Wait()
	os.Exit(nFailed)
}

func execTests(tests []test, nrVMs int, vmPool chan vmInstance) (int, error) {
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
			var same bool
			for _, s := range testGrp.SameVMs {
				if s == t {
					same = true
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
			go func(st string, same bool, controler vmInstance, testnodes ...vmInstance) {
				defer testGrpWG.Done()

				stTest := time.Now()
				testRes := execTest(ctx, st, same, vmPool, controller, testnodes...)
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
			}(t, same, controller, vms...)

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

func execTest(ctx context.Context, test string, same bool, vmPool chan<- vmInstance, controller vmInstance, testnodes ...vmInstance) *testResult {
	defer func() {
		vmPool <- controller
		for _, vm := range testnodes {
			vmPool <- vm
		}
	}()

	res := newTestResult(cmdName)

	// always also print the header
	testInstance := fmt.Sprintf("%s-%d", test, len(testnodes))
	res.AppendLog(false, "EXECUTING: %s in %+v Nodes(%+v)", testInstance, controller, testnodes)

	// Start VMs
	start := time.Now()
	err := startVMs(test, res, same, controller, testnodes...)
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

	start = time.Now()
	//TODO(rck): This is specific to the drbd 9 tests, factor that out...
	payload := fmt.Sprintf("d9ts:leader:tests=%s:undertest=%s",
		test, strings.Join(testvms, ","))
	argv := []string{"ssh", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
		fmt.Sprintf("root@vm-%d", controller.nr), "/payloads/d9ts", payload}

	res.AppendLog(*quiet, "EXECUTING the actual test via ssh: %s", argv)

	ctx, cancel := context.WithTimeout(ctx, *testTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out, testErr := cmd.CombinedOutput()
	res.AppendInVM(true, "%s\n", out)
	res.AppendLog(*quiet, "EXECUTIONTIME: %s %v", testInstance, time.Since(start))
	if testErr != nil {
		res.err = fmt.Errorf("ERROR: %s %v", testInstance, testErr)
		return res
	}
	res.AppendLog(*quiet, "SUCCESS: %s", testInstance)

	return res
}

// no parent ctx, we always (try) to do that
// ch2vm has a lot of "intermediate state" (maybe too much). if we kill it "in the middle" we might for example end up with zfs leftovers
// start and tear down are fast enough...
func startVMs(test string, res *testResult, same bool, controller vmInstance, testnodes ...vmInstance) error {
	allVMs := []vmInstance{controller}
	if same {
		for _, n := range testnodes {
			// vm := n
			n.Distribution = controller.Distribution
			n.Kernel = controller.Kernel
			allVMs = append(allVMs, n)
		}
	} else {
		allVMs = append(allVMs, testnodes...)
	}

	for _, n := range allVMs {
		unitName := unitName(n)

		// clean up, should not be neccessary, but hey...
		argv := []string{"systemctl", "reset-failed", unitName + ".scope"}
		res.AppendLog(*quiet, "EXECUTING: %s", argv)
		// we don't care for the outcome, in be best case it helped, otherwise start will fail
		exec.Command(argv[0], argv[1:]...).Run()

		// TODO(rck:) This is the test specific part, if we integrate more projects, factor that out.
		payloads := "sshd;shell"
		if n.nr != controller.nr {
			payloads = "lvm;networking;loaddrbd;" + payloads
		} else if isJenkins() {
			jdir := filepath.Join(*jenkins, "log", fmt.Sprintf("%s-%d", test, len(allVMs)-1))
			payloads += fmt.Sprintf(";jenkins:jdir=%s:jtest=%s", jdir, test)
		}
		argv = []string{"systemd-run", "--unit=" + unitName, "--scope",
			"./ch2vm.sh", "-d", n.Distribution, "-k", n.Kernel, "-v", fmt.Sprintf("%d", n.nr), payloads}

		res.AppendLog(*quiet, "EXECUTING: %s", argv)
		cmd := exec.Command(argv[0], argv[1:]...)
		if err := cmd.Start(); err != nil {
			return err
		}

		systemdScope.Add(1)
		go func(cmd *exec.Cmd) {
			defer systemdScope.Done()
			cmd.Wait()
		}(cmd)
	}

	return nil
}

func sshPing(ctx context.Context, res *testResult, controller vmInstance) error {
	controllerVM := fmt.Sprintf("vm-%d", controller.nr)
	argv := []string{"ssh", "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=1", "-o", "ConnectTimeout=1",
		"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
		controllerVM, "true"}

	start := time.Now()
	for {
		if ctxCancled(ctx) {
			return fmt.Errorf("ERROR: Gave up SSH-pinging controller node %s (ctx cancled)", controllerVM)
		}
		err := exec.Command(argv[0], argv[1:]...).Run()
		if err == nil {
			break
		}
		// log.Printf(".")
		if time.Since(start) > *sshTimeout {
			return fmt.Errorf("ERROR: Gave up SSH-pinging controller node %s (timeout)", controllerVM)
		}
		time.Sleep(1 * time.Second)
	}

	return nil
}

// no parent ctx, we always (try) to do that
func shutdownVMs(res *testResult, controller vmInstance, testnodes ...vmInstance) error {
	allVMs := []vmInstance{controller}
	allVMs = append(allVMs, testnodes...)

	for _, n := range allVMs {
		unitName := unitName(n)

		argv := []string{"systemctl", "stop", unitName + ".scope"}
		res.AppendLog(*quiet, "EXECUTING: %s", argv)
		if err := exec.Command(argv[0], argv[1:]...).Run(); err != nil {
			res.AppendLog(*quiet, "ERROR: Could not stop unit %s %v", unitName, err)
			// do not return, keep going...
		}
	}
	res.AppendLog(*quiet, "Waited for VMs")

	return nil
}

func unitName(vm vmInstance) string {
	return fmt.Sprintf("LBTEST-vm-%d", vm.nr)
}

func ctxCancled(ctx context.Context) bool {
	return ctx.Err() != nil
}

func isJenkins() bool { return *jenkins != "" }

func jenkinsSubDir(subdir string) (string, error) {
	if !isJenkins() {
		return "", errors.New("This is not a jenkins run")
	}
	p := filepath.Join(*jenkins, subdir)

	if st, err := os.Stat(p); err == nil && st.IsDir() {
		return p, nil
	}

	return p, os.MkdirAll(p, 0755)
}

func jenkinsLog(test, name string, buf *bytes.Buffer) error {
	p, err := jenkinsSubDir(test)
	if err != nil {
		return err
	}

	return ioutil.WriteFile(filepath.Join(p, name), buf.Bytes(), 0644)
}

func jenkinsXMLLog(restultsDir, name string, testRes *testResult, buf *bytes.Buffer) error {
	re, err := regexp.Compile("[^\t\n\r\x20-\x7e]")
	if err != nil {
		return err
	}

	p, err := jenkinsSubDir(restultsDir)

	f, err := os.Create(filepath.Join(p, name+".xml"))
	if err != nil {
		return err
	}
	defer f.Close()

	var nrFailed int
	if testRes.err != nil {
		nrFailed = 1 // currently there is only one test per execution
	}
	// header := fmt.Sprintf("<?xml version=\"1.0\" encoding=\"UTF-8\"?><testsuite tests=\"1\" failures=\"0\" errors=\"%d\">\n", status)
	header := fmt.Sprintf("<testsuite tests=\"1\" failures=\"%d\" assertions=\"1\">\n", nrFailed)
	header += fmt.Sprintf("<testcase classname=\"test.%s\" name=\"%s.run\" time=\"%.2f\">", name, name, testRes.execTime.Seconds())
	header += "<system-out>\n<![CDATA[\n"
	f.WriteString(header)
	f.Write(re.ReplaceAllLiteral(buf.Bytes(), []byte{' '}))
	f.WriteString("]]></system-out>\n")
	if nrFailed > 0 {
		f.WriteString("<failure message=\"FAILED\"/>\n")
	}
	f.WriteString("</testcase></testsuite>")

	return nil
}
