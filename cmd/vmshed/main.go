package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/nightlyone/lockfile"
)

var zfsVMs []vm

var systemdScope sync.WaitGroup

var (
	cmdName = filepath.Base(os.Args[0])

	vmSpec      = flag.String("vms", "vms.json", "File containing VM specification")
	testSpec    = flag.String("tests", "tests.json", "File containing test specification")
	testSuite   = flag.String("suite", "drbd9", "Test suite to start")
	startVM     = flag.Int("startvm", 1, "Number of the first VM to start in parallel")
	nrVMs       = flag.Int("nvms", 12, "Maximum number of VMs to start in parallel, starting at -startvm")
	failTest    = flag.Bool("failtest", false, "Stop executing tests when the first one failed")
	failGrp     = flag.Bool("failgroup", false, "Stop executing tests when at least one failed in the test group")
	quiet       = flag.Bool("quiet", false, "Don't print progess messages while tests are running")
	jenkins     = flag.String("jenkins", "", "If this is set to a path for the current job, text output is saved to files, logs get copied,...")
	sshTimeout  = flag.Duration("sshping", 3*time.Minute, "Timeout for ssh pinging the controller node")
	testTimeout = flag.Duration("testtime", 5*time.Minute, "Timeout for a single test execution in a VM")
	ctrlDist    = flag.String("ctrldist", "", "If this is set, use this distribution for the controller VM, needs ctrlkernel set")
	ctrlKernel  = flag.String("ctrlkernel", "", "If this is set, use this kernel for the controller VM, needs ctrldist set")
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
		if vm.HasZFS {
			zfsVMs = append(zfsVMs, vm)
		}
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
		vm.HasZFS = rndVM.HasZFS

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

	if isJenkins() {
		// transfer ownership to Jenkins, so that the workspace can be cleaned before running again
		err := jenkinsSetOwner(*jenkins)
		if err != nil {
			log.Println(cmdName, "ERROR SETTING WORKSPACE OWNERSHIP:", err)
		}
	}

	os.Exit(nFailed)
}

func finalVMs(to testOption, origController vmInstance, origTestnodes ...vmInstance) (vmInstance, []vmInstance, error) {
	controller := origController
	testnodes := origTestnodes

	if to.needsZFS && len(zfsVMs) == 0 {
		return controller, testnodes, errors.New("You required ZFS, but none of your test VMs supports it")
	}

	if *ctrlDist != "" && *ctrlKernel != "" {
		controller.Distribution = *ctrlDist
		controller.Kernel = *ctrlKernel
	}
	for _, n := range zfsVMs {
		if controller.Distribution == n.Distribution && controller.Kernel == n.Kernel {
			controller.HasZFS = true
			break
		}
	}

	if to.needsSameVMs {
		if to.needsZFS {
			if !controller.HasZFS {
				return controller, testnodes, fmt.Errorf("Controller node (%s:%s) does not have ZFS support", controller.Distribution, controller.Kernel)
			}
		}
		for i := range testnodes {
			testnodes[i].Distribution = controller.Distribution
			testnodes[i].Kernel = controller.Kernel
			testnodes[i].HasZFS = controller.HasZFS
		}
	} else if to.needsZFS {
		for i := range testnodes {
			zfsNode := zfsVMs[rand.Int31n(int32(len(zfsVMs)))]
			testnodes[i].Distribution = zfsNode.Distribution
			testnodes[i].Kernel = zfsNode.Kernel
			testnodes[i].HasZFS = zfsNode.HasZFS
		}
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

func unitName(vm vmInstance) string {
	return fmt.Sprintf("LBTEST-vm-%d-%s", vm.nr, vm.CurrentUUID)
}

func ctxCancled(ctx context.Context) bool {
	return ctx.Err() != nil
}
