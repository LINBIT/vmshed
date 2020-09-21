package cmd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

type vm struct {
	BaseImage string            `toml:"base_image"`
	Values    map[string]string `toml:"values"`
	Memory    string            `toml:"memory"`
	VCPUs     uint              `toml:"vcpus"`
	Disks     []string          `toml:"disks"`
}

type vmInstance struct {
	ImageName string
	nr        int
	memory    string
	vcpus     uint
	disks     []string
}

func (vm vmInstance) vmName() string {
	return fmt.Sprintf("lbtest-vm-%d", vm.nr)
}

func testIDString(test string, vmCount int, testIndex int) string {
	return fmt.Sprintf("%s-%d-%d", test, vmCount, testIndex)
}

func provisionImage(vmSpec *vmSpecification, overrides []string, nr int, v *vm, jenkins *Jenkins) error {
	newImageName := vmSpec.ImageName(v)

	// clean up, should not be neccessary, but hey...
	argv := []string{"virter", "image", "rm", newImageName}
	log.Printf("EXECUTING: %s", argv)
	// this command is idempotent, so even if it does nothing, it returns zero
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = virterEnv()
	if err := cmd.Run(); err != nil {
		return err
	}

	argv = []string{"virter", "image", "build",
		"--id", strconv.Itoa(nr),
		"--provision", vmSpec.ProvisionFile}
	if jenkins.IsActive() {
		argv = append(argv, "--console", jenkins.SubDir("provision-log"))
	}
	for _, override := range overrides {
		argv = append(argv, "--set", override)
	}
	for key, value := range v.Values {
		argv = append(argv, "--set", "values."+key+"="+value)
	}
	argv = append(argv, v.BaseImage, newImageName)

	cmd = exec.Command(argv[0], argv[1:]...)
	cmd.Env = virterEnv()

	var out bytes.Buffer
	cmd.Stderr = &out

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(vmSpec.ProvisionTimeout))
	defer cancel()

	log.Printf("EXECUTING: %s", argv)
	start := time.Now()
	err := cmdRunTerm(ctx, log.StandardLogger(), cmd)
	log.Printf("EXECUTIONTIME: Provisioning image %s: %v", newImageName, time.Since(start))

	if exitErr, ok := err.(*exec.ExitError); ok {
		exitErr.Stderr = out.Bytes()
	}
	return err
}

func removeImages(vmSpec *vmSpecification) {
	if vmSpec.ProvisionFile == "" {
		return
	}

	for _, v := range vmSpec.VMs {
		newImageName := vmSpec.ImageName(&v)

		argv := []string{"virter", "image", "rm", newImageName}
		log.Printf("EXECUTING: %s", argv)
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Env = virterEnv()
		if stdouterr, err := cmd.CombinedOutput(); err != nil {
			log.Errorf("ERROR: Could not remove image %s %v: stdouterr: %s", newImageName, err, stdouterr)
			// do not return, keep going...
		}
	}
}

func startVMs(logger *log.Logger, run *testRun, testnodes ...vmInstance) error {
	var vmStartWait sync.WaitGroup
	errCh := make(chan error, len(testnodes))

	for _, vm := range testnodes {
		vmStartWait.Add(1)
		go func(vm vmInstance) {
			defer vmStartWait.Done()
			if err := runVM(logger, run, vm); err != nil {
				errCh <- err
			}
		}(vm)
	}

	vmStartWait.Wait()
	close(errCh)

	// return the first error, if any
	err := <-errCh
	return err
}

func runVM(logger *log.Logger, run *testRun, vm vmInstance) error {
	vmName := vm.vmName()

	// clean up, should not be neccessary, but hey...
	argv := []string{"virter", "vm", "rm", vmName}
	logger.Printf("EXECUTING: %s", argv)
	// this command is idempotent, so even if it does nothing, it returns zero
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = virterEnv()
	if err := cmd.Run(); err != nil {
		return err
	}

	argv = []string{"virter", "vm", "run",
		"--name", vmName,
		"--id", strconv.Itoa(vm.nr),
		"--console", run.outDir,
		"--memory", vm.memory,
		"--vcpus", strconv.Itoa(int(vm.vcpus))}

	for _, disks := range vm.disks {
		argv = append(argv, "--disk", disks)
	}
	argv = append(argv, "--wait-ssh", vm.ImageName)

	logger.Printf("EXECUTING: %s", argv)
	cmd = exec.Command(argv[0], argv[1:]...)
	cmd.Env = virterEnv()

	// use Output to capture stderr if the exit code is non-zero
	_, err := cmd.Output()
	return err
}

func shutdownVMs(logger *log.Logger, testnodes ...vmInstance) error {
	for _, vm := range testnodes {
		vmName := vm.vmName()

		argv := []string{"virter", "vm", "rm", vmName}
		logger.Printf("EXECUTING: %s", argv)
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Env = virterEnv()
		if stdouterr, err := cmd.CombinedOutput(); err != nil {
			logger.Errorf("ERROR: Could not stop VM %s %v: stdouterr: %s", vmName, err, stdouterr)
			// do not return, keep going...
		}
	}

	return nil
}

func virterEnv() []string {
	return append(os.Environ(), "LIBVIRT_STATIC_DHCP=true")
}

// cmdRunTerm runs a Cmd, terminating it gracefully when the context is done
func cmdRunTerm(ctx context.Context, logger *log.Logger, cmd *exec.Cmd) error {
	err := cmd.Start()
	if err != nil {
		return err
	}

	complete := make(chan struct{})
	finished := make(chan struct{})

	go handleTermination(ctx, logger, cmd, complete, finished)

	err = cmd.Wait()

	// Inform the termination handler that it can stop
	close(complete)

	// Wait for the termination handler to stop, so that the context can be
	// cancelled without risk of sending an extra signal
	<-finished

	return err
}

func handleTermination(ctx context.Context, logger *log.Logger, cmd *exec.Cmd, complete <-chan struct{}, finished chan<- struct{}) {
	select {
	case <-ctx.Done():
		logger.Warnln("TERMINATING: Send SIGTERM")
		cmd.Process.Signal(unix.SIGTERM)
		select {
		case <-time.After(10 * time.Second):
			logger.Errorln("TERMINATING: Send SIGKILL")
			cmd.Process.Kill()
		case <-complete:
		}
	case <-complete:
	}
	close(finished)
}
