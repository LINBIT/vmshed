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

func provisionImage(ctx context.Context, vmSpec *vmSpecification, overrides []string, nr int, v *vm, jenkins *Jenkins) error {
	newImageName := vmSpec.ImageName(v)
	logger := log.WithFields(log.Fields{
		"Action":    "Provision",
		"ImageName": newImageName,
	})

	// clean up, should not be neccessary, but hey...
	argv := []string{"virter", "image", "rm", newImageName}
	log.Printf("EXECUTING: %s", argv)
	// this command is idempotent, so even if it does nothing, it returns zero
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = virterEnv()
	if err := cmdStderrTerm(ctx, logger, cmd); err != nil {
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

	provisionCtx, cancel := context.WithTimeout(ctx, time.Duration(vmSpec.ProvisionTimeout))
	defer cancel()

	log.Printf("EXECUTING: %s", argv)
	start := time.Now()
	err := cmdStderrTerm(provisionCtx, logger, cmd)
	log.Printf("EXECUTIONTIME: Provisioning image %s: %v", newImageName, time.Since(start))

	return err
}

func removeImages(vmSpec *vmSpecification) {
	if vmSpec.ProvisionFile == "" {
		return
	}

	for _, v := range vmSpec.VMs {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		newImageName := vmSpec.ImageName(&v)

		// remove with "vm rm" in case the build failed leaving the provisioning VM running
		argv := []string{"virter", "vm", "rm", newImageName}
		log.Printf("EXECUTING: %s", argv)
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Env = virterEnv()
		if err := cmdStderrTerm(ctx, log.StandardLogger(), cmd); err != nil {
			log.Errorf("ERROR: Could not remove image %s %v", newImageName, err)
			dumpStderr(log.StandardLogger(), err)
			// do not return, keep going...
		}
	}
}

func startVMs(ctx context.Context, logger *log.Logger, run *testRun, testnodes ...vmInstance) error {
	var vmStartWait sync.WaitGroup
	errCh := make(chan error, len(testnodes))

	for _, vm := range testnodes {
		vmStartWait.Add(1)
		go func(vm vmInstance) {
			defer vmStartWait.Done()
			if err := runVM(ctx, logger, run, vm); err != nil {
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

func runVM(ctx context.Context, logger *log.Logger, run *testRun, vm vmInstance) error {
	vmName := vm.vmName()

	// clean up, should not be neccessary, but hey...
	argv := []string{"virter", "vm", "rm", vmName}
	logger.Printf("EXECUTING: %s", argv)
	// this command is idempotent, so even if it does nothing, it returns zero
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = virterEnv()
	if err := cmdStderrTerm(ctx, logger, cmd); err != nil {
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
	return cmdStderrTerm(ctx, logger, cmd)
}

func shutdownVMs(logger *log.Logger, testnodes ...vmInstance) error {
	for _, vm := range testnodes {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		vmName := vm.vmName()

		argv := []string{"virter", "vm", "rm", vmName}
		logger.Printf("EXECUTING: %s", argv)
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Env = virterEnv()
		if err := cmdStderrTerm(ctx, logger, cmd); err != nil {
			logger.Errorf("ERROR: Could not stop VM %s: %v", vmName, err)
			dumpStderr(logger, err)
			// do not return, keep going...
		}
	}

	return nil
}

func virterEnv() []string {
	return append(os.Environ(), "LIBVIRT_STATIC_DHCP=true")
}

// cmdStderrTerm runs a Cmd, collecting stderr and terminating gracefully
func cmdStderrTerm(ctx context.Context, logger log.FieldLogger, cmd *exec.Cmd) error {
	var out bytes.Buffer
	cmd.Stderr = &out

	err := cmdRunTerm(ctx, logger, cmd)

	if exitErr, ok := err.(*exec.ExitError); ok {
		exitErr.Stderr = out.Bytes()
	}

	return err
}

// cmdRunTerm runs a Cmd, terminating it gracefully when the context is done
func cmdRunTerm(ctx context.Context, logger log.FieldLogger, cmd *exec.Cmd) error {
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

func handleTermination(ctx context.Context, logger log.FieldLogger, cmd *exec.Cmd, complete <-chan struct{}, finished chan<- struct{}) {
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

func dumpStderr(logger *log.Logger, err error) {
	if exitErr, ok := err.(*exec.ExitError); ok {
		fmt.Fprint(logger.Out, string(exitErr.Stderr))
	}
}
