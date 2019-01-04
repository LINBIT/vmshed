package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
)

type vm struct {
	Distribution string `json:"distribution"`
	Kernel       string `json:"kernel"`
	HasZFS       bool   `json:"zfs"`
}

type vmInstance struct {
	nr          int
	CurrentUUID string
	vm
}

func (vm vmInstance) unitName() string {
	return fmt.Sprintf("LBTEST-vm-%d-%s", vm.nr, vm.CurrentUUID)
}

// no parent ctx, we always (try) to do that
// ch2vm has a lot of "intermediate state" (maybe too much). if we kill it "in the middle" we might for example end up with zfs leftovers
// start and tear down are fast enough...
func startVMs(test string, res *testResult, to testOption, controller vmInstance, testnodes ...vmInstance) error {
	allVMs := []vmInstance{controller}
	allVMs = append(allVMs, testnodes...)
	for _, vm := range allVMs {
		unitName := vm.unitName()

		// clean up, should not be neccessary, but hey...
		argv := []string{"systemctl", "reset-failed", unitName + ".scope"}
		res.AppendLog(*quiet, "EXECUTING: %s", argv)
		// we don't care for the outcome, in be best case it helped, otherwise start will fail
		exec.Command(argv[0], argv[1:]...).Run()

		payloads := "sshd;shell"
		if vm.nr != controller.nr {
			op := payloads
			pool := "lvm"
			if to.needsZFS {
				pool = "zfs"
			}
			payloads = fmt.Sprintf("%s;networking;loaddrbd;", pool)
			if *testSuite == "linstor" {
				payloads += "linstor:combined;"
			}
			payloads += op
		}
		argv = []string{"systemd-run", "--unit=" + unitName, "--scope",
			"./ch2vm.sh", "-s", *testSuite, "-d", vm.Distribution, "-k", vm.Kernel,
			"--uuid", vm.CurrentUUID,
			"-v", fmt.Sprintf("%d", vm.nr), "-p", payloads}

		if jenkins.IsActive() {
			jdir := filepath.Join(jenkins.Workspace(), "log", fmt.Sprintf("%s-%d", test, len(allVMs)-1))
			argv = append(argv, fmt.Sprintf("--jdir=%s", jdir))
			argv = append(argv, fmt.Sprintf("--jtest=%s", test))
		}

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

// no parent ctx, we always (try) to do that
func shutdownVMs(res *testResult, controller vmInstance, testnodes ...vmInstance) error {
	allVMs := []vmInstance{controller}
	allVMs = append(allVMs, testnodes...)

	for _, vm := range allVMs {
		unitName := vm.unitName()

		argv := []string{"systemctl", "stop", unitName + ".scope"}
		res.AppendLog(*quiet, "EXECUTING: %s", argv)
		if stdouterr, err := exec.Command(argv[0], argv[1:]...).CombinedOutput(); err != nil {
			res.AppendLog(*quiet, "ERROR: Could not stop unit %s %v: stdouterr: %s", unitName, err, stdouterr)
			// do not return, keep going...
		}
	}
	res.AppendLog(*quiet, "Waited for VMs")

	return nil
}
