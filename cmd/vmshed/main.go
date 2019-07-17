package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/LINBIT/lbtest/cmd/vmshed/config"
	"github.com/nightlyone/lockfile"
)

var allVMs []vm

var jenkins *Jenkins

var (
	cmdName = filepath.Base(os.Args[0])

	vmSpec      = flag.String("vms", "vms.json", "File containing VM specification")
	testSpec    = flag.String("tests", "tests.json", "File containing test specification")
	toRun       = flag.String("torun", "all", "comma separated list of test names to execute ('all' is a reserved test name)")
	testSuite   = flag.String("suite", "drbd9", "Test suite to start")
	startVM     = flag.Int("startvm", 1, "Number of the first VM to start in parallel")
	nrVMs       = flag.Int("nvms", 12, "Maximum number of VMs to start in parallel, starting at -startvm")
	failTest    = flag.Bool("failtest", false, "Stop executing tests when the first one failed")
	failGrp     = flag.Bool("failgroup", false, "Stop executing tests when at least one failed in the test group")
	quiet       = flag.Bool("quiet", false, "Don't print progess messages while tests are running")
	jenkinsWS   = flag.String("jenkins", "", "If this is set to a path for the current job, text output is saved to files, logs get copied,...")
	sshTimeout  = flag.Duration("sshping", 3*time.Minute, "Timeout for ssh pinging the controller node")
	testTimeout = flag.Duration("testtime", 5*time.Minute, "Timeout for a single test execution in a VM")
	ctrlDist    = flag.String("ctrldist", "", "If this is set, use this distribution for the controller VM, needs ctrlkernel set")
	ctrlKernel  = flag.String("ctrlkernel", "", "If this is set, use this kernel for the controller VM, needs ctrldist set")
	version     = flag.Bool("version", false, "Print version and exit")
)

func main() {
	flag.Parse()
	prog := path.Base(os.Args[0])

	if *version {
		fmt.Println(prog, config.Version)
		return
	}

	if *startVM <= 0 {
		log.Fatal(cmdName, "-startvm has to be positive")
	}
	if *nrVMs <= 0 {
		log.Fatal(cmdName, "-nvms has to be positive")
	}

	jenkins = NewJenkinsMust(*jenkinsWS)

	vmFile, err := os.Open(*vmSpec)
	if err != nil {
		log.Fatal(err)
	}

	dec := json.NewDecoder(vmFile)

	for {
		var vm vm
		if err := dec.Decode(&vm); err == io.EOF {
			break
		} else if err != nil {
			log.Fatal(err)
		}

		if vm.HasZFS {
			vm.vmcap |= zfs
		}
		if vm.HasMariaDB {
			vm.vmcap |= mariaDB
		}
		if vm.HasPostgres {
			vm.vmcap |= postgres
		}

		allVMs = append(allVMs, vm)
	}

	testFile, err := os.Open(*testSpec)
	if err != nil {
		log.Fatal(err)
	}

	dec = json.NewDecoder(testFile)

	var tests []testGroup
	for {
		var test testGroup
		if err := dec.Decode(&test); err == io.EOF {
			break
		} else if err != nil {
			log.Fatal(err)
		}

		if *toRun != "all" && *toRun != "" { //filter tests
			idx := 0
			for _, tn := range test.Tests {
				for _, vt := range strings.Split(*toRun, ",") {
					if tn == vt {
						test.Tests[idx] = tn
						idx++
					}
				}
			}
			test.Tests = test.Tests[:idx]
		}

		if len(test.Tests) == 0 {
			continue
		}

		tests = append(tests, test)
	}

	// generate random VMs
	vmPool := make(chan vmInstance, *nrVMs)
	for i := 0; i < *nrVMs; i++ {
		var vm vmInstance
		vm.nr = i + *startVM
		r, err := rand.Int(rand.Reader, big.NewInt(int64(len(allVMs))))
		if err != nil {
			log.Fatal(err)
		}
		rndVM := allVMs[r.Int64()]
		vm.Distribution = rndVM.Distribution
		vm.Kernel = rndVM.Kernel
		vm.vmcap = rndVM.vmcap

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

	// transfer ownership to Jenkins, so that the workspace can be cleaned before running again
	if err := jenkins.OwnWorkspace(); err != nil {
		log.Println(cmdName, "ERROR SETTING WORKSPACE OWNERSHIP:", err)
	}

	os.Exit(nFailed)
}

func finalVMs(to testOption, origController vmInstance, origTestnodes ...vmInstance) (vmInstance, []vmInstance, error) {
	controller := origController
	testnodes := origTestnodes

	var reqVMCaps vmcap
	if to.needsZFS {
		reqVMCaps |= zfs
	}
	if to.needsMariaDB {
		reqVMCaps |= mariaDB
	}
	if to.needsPostgres {
		reqVMCaps |= postgres
	}

	if *ctrlDist != "" && *ctrlKernel != "" {
		controller.Distribution = *ctrlDist
		controller.Kernel = *ctrlKernel

		for _, n := range allVMs {
			if controller.Distribution == n.Distribution && controller.Kernel == n.Kernel {
				controller.vmcap = n.vmcap
				break
			}
		}
	}

	if to.needsSameVMs { // sameVMs includes the controller
		if !reqVMCaps.fulfilledby(controller.vmcap) {
			return controller, testnodes, fmt.Errorf("Controller node (%s:%s) does not support all required properties", controller.Distribution, controller.Kernel)
		}
		for i := range testnodes {
			testnodes[i].Distribution = controller.Distribution
			testnodes[i].Kernel = controller.Kernel
			testnodes[i].vmcap = controller.vmcap
		}
	} else if to.needsAllPlatforms { // this only includes the nodes under test
		oneVM := allVMs[to.platformIdx]
		if !reqVMCaps.fulfilledby(controller.vmcap) {
			return controller, testnodes, fmt.Errorf("One selected node (%s:%s) does not have all required caps support", oneVM.Distribution, oneVM.Kernel)
		}
		for i := range testnodes {
			testnodes[i].Distribution = oneVM.Distribution
			testnodes[i].Kernel = oneVM.Kernel
			testnodes[i].vmcap = oneVM.vmcap
		}
	} else if reqVMCaps != 0 { // find matching VMs.
		capVMs := make([]vm, 0, len(testnodes))

		for i := range allVMs {
			if reqVMCaps.fulfilledby(allVMs[i].vmcap) {
				capVMs = append(capVMs, allVMs[i])
			}
		}

		for i := range testnodes {
			r, err := rand.Int(rand.Reader, big.NewInt(int64(len(capVMs))))
			if err != nil {
				log.Fatal(err)
			}
			selNode := capVMs[r.Int64()]
			testnodes[i].Distribution = selNode.Distribution
			testnodes[i].Kernel = selNode.Kernel
			testnodes[i].vmcap = selNode.vmcap
		}
	}

	controller.vm.setHasByCap()
	for i := range testnodes {
		testnodes[i].vm.setHasByCap()
	}

	return controller, testnodes, nil
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

func ctxCancled(ctx context.Context) bool {
	return ctx.Err() != nil
}
