package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

type vmcap uint

const (
	zfs vmcap = 1 << iota
	mariaDB
	postgres
	etcd
)

func (c vmcap) isSet(tcap vmcap) bool {
	return c&tcap != 0
}

func (req vmcap) fulfilledby(some vmcap) bool {
	if (req & some) == req {
		return true
	}
	return false
}

type vm struct {
	Distribution string `json:"distribution"`
	Kernel       string `json:"kernel"`
	HasZFS       bool   `json:"zfs"`
	HasMariaDB   bool   `json:"mariadb"`
	HasPostgres  bool   `json:"postgres"`
	HasETCd      bool   `json:"etcd"`

	vmcap vmcap
}

func (v vm) setHasByCap() {
	v.HasZFS = v.vmcap.isSet(zfs)
	v.HasMariaDB = v.vmcap.isSet(mariaDB)
	v.HasPostgres = v.vmcap.isSet(postgres)
	v.HasETCd = v.vmcap.isSet(etcd)
}

type vmInstance struct {
	nr          int
	CurrentUUID string
	vm
}

func (vm vmInstance) unitName() string {
	return fmt.Sprintf("LBTEST-vm-%d-%s", vm.nr, vm.CurrentUUID)
}

func testIdString(test string, needsETCd bool, vmCount int, platformIdx int) string {
	return fmt.Sprintf("%s-etcd-%t-%d-%d", test, needsETCd, vmCount, platformIdx)
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
			pool := "lvm:thinpercent=20"
			if to.needsZFS {
				pool = "zfs"
			}
			payloads = fmt.Sprintf("%s;networking;loaddrbd;", pool)
			if *testSuite == "linstor" || *testSuite == "golinstor" {
				var lsetcd string
				if to.needsETCd {
					lsetcd = ":etcd"
				}
				payloads += fmt.Sprintf("linstor:combined%s;", lsetcd)
				if to.needsPostgres {
					payloads += "db:postgres;"
				}
				if to.needsMariaDB {
					payloads += "db:mariadb;"
				}
				if to.needsETCd {
					payloads += "db:etcd;"
				}
			} else if *testSuite == "drbdproxy" {
				payloads += "drbdproxy;"
			}
			payloads += op
		}
		argv = []string{"systemd-run", "--unit=" + unitName, "--scope",
			"./ch2vm.sh", "-s", *testSuite, "-d", vm.Distribution, "-k", vm.Kernel,
			"--uuid", vm.CurrentUUID,
			"-v", fmt.Sprintf("%d", vm.nr), "-p", payloads}

		var stdout *os.File
		var stderr *os.File
		if jenkins.IsActive() {
			testOut := testIdString(test, to.needsETCd, len(allVMs)-1, to.platformIdx)
			jdir := jenkins.LogDir(testOut)
			argv = append(argv, fmt.Sprintf("--jdir=%s", jdir))
			argv = append(argv, fmt.Sprintf("--jtest=%s", test))

			vmOutDir := filepath.Join("log", testOut, "outsideVM")

			var err error
			stdout, err = jenkins.CreateFile(vmOutDir, fmt.Sprintf("vm-%d-stdout.log", vm.nr))
			if err != nil {
				return err
			}

			stderr, err = jenkins.CreateFile(vmOutDir, fmt.Sprintf("vm-%d-stderr.log", vm.nr))
			if err != nil {
				return err
			}
		}

		res.AppendLog(*quiet, "EXECUTING: %s", argv)
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdout = stdout
		cmd.Stderr = stderr

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
