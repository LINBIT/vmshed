package cmd

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/nightlyone/lockfile"
	"github.com/spf13/cobra"

	"github.com/LINBIT/vmshed/cmd/config"
)

type vmSpecification struct {
	Name          string `toml:"name"`
	ProvisionFile string `toml:"provision_file"`
	VMs           []vm   `toml:"vms"`
}

func (s *vmSpecification) ImageName(v vm) string {
	if s.ProvisionFile == "" {
		// No provisioning, use base image directly
		return v.BaseImage
	}
	return fmt.Sprintf("%s-%s", v.BaseImage, s.Name)
}

type testSpecification struct {
	TestSuiteFile string      `toml:"test_suite_file"`
	TestGroups    []testGroup `toml:"group"`
}

type TestRun struct {
	cmdName     string
	vmSpec      *vmSpecification
	testSpec    *testSpecification
	jenkins     *Jenkins
	nrVMs       int
	failTest    bool
	failGrp     bool
	quiet       bool
	testTimeout time.Duration
}

// Execute runs vmshed
func Execute() {
	if err := rootCommand().Execute(); err != nil {
		log.Fatal(err)
	}
}

func rootCommand() *cobra.Command {
	cmdName := filepath.Base(os.Args[0])
	prog := path.Base(os.Args[0])

	var vmSpecPath string
	var testSpecPath string
	var toRun string
	var startVM int
	var nrVMs int
	var failTest bool
	var failGrp bool
	var quiet bool
	var jenkinsWS string
	var testTimeout time.Duration
	var version bool

	rootCmd := &cobra.Command{
		Use:   "vmshed",
		Short: "Run tests in VMs",
		Long:  `Run tests in VMs`,
		Run: func(cmd *cobra.Command, args []string) {
			if version {
				fmt.Println(prog, config.Version)
				return
			}

			if startVM <= 0 {
				log.Fatal(cmdName, "--startvm has to be positive")
			}
			if nrVMs <= 0 {
				log.Fatal(cmdName, "--nvms has to be positive")
			}

			jenkins := NewJenkinsMust(jenkinsWS)

			var vmSpec vmSpecification
			if _, err := toml.DecodeFile(vmSpecPath, &vmSpec); err != nil {
				log.Fatal(err)
			}
			vmSpec.ProvisionFile = joinIfRel(filepath.Dir(vmSpecPath), vmSpec.ProvisionFile)

			var testSpec testSpecification
			if _, err := toml.DecodeFile(testSpecPath, &testSpec); err != nil {
				log.Fatal(err)
			}
			testSpec.TestSuiteFile = joinIfRel(filepath.Dir(testSpecPath), testSpec.TestSuiteFile)

			var tests []testGroup
			for _, test := range testSpec.TestGroups {
				if toRun != "all" && toRun != "" { //filter tests
					idx := 0
					for _, tn := range test.Tests {
						for _, vt := range strings.Split(toRun, ",") {
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
			testSpec.TestGroups = tests

			start := time.Now()

			testRun := TestRun{
				cmdName:     cmdName,
				vmSpec:      &vmSpec,
				testSpec:    &testSpec,
				jenkins:     jenkins,
				nrVMs:       nrVMs,
				failTest:    failTest,
				failGrp:     failGrp,
				quiet:       quiet,
				testTimeout: testTimeout,
			}
			nFailed := provisionAndExec(&testRun, startVM)

			log.Println(cmdName, "OVERALL EXECUTIONTIME:", time.Since(start))

			// transfer ownership to Jenkins, so that the workspace can be cleaned before running again
			if err := jenkins.OwnWorkspace(); err != nil {
				log.Println(cmdName, "ERROR SETTING WORKSPACE OWNERSHIP:", err)
			}

			os.Exit(nFailed)
		},
	}

	rootCmd.Flags().StringVarP(&vmSpecPath, "vms", "", "vms.toml", "File containing VM specification")
	rootCmd.Flags().StringVarP(&testSpecPath, "tests", "", "tests.toml", "File containing test specification")
	rootCmd.Flags().StringVarP(&toRun, "torun", "", "all", "comma separated list of test names to execute ('all' is a reserved test name)")
	rootCmd.Flags().IntVarP(&startVM, "startvm", "", 2, "Number of the first VM to start in parallel")
	rootCmd.Flags().IntVarP(&nrVMs, "nvms", "", 12, "Maximum number of VMs to start in parallel, starting at -startvm")
	rootCmd.Flags().BoolVarP(&failTest, "failtest", "", false, "Stop executing tests when the first one failed")
	rootCmd.Flags().BoolVarP(&failGrp, "failgroup", "", false, "Stop executing tests when at least one failed in the test group")
	rootCmd.Flags().BoolVarP(&quiet, "quiet", "", false, "Don't print progess messages while tests are running")
	rootCmd.Flags().StringVarP(&jenkinsWS, "jenkins", "", "", "If this is set to a path for the current job, text output is saved to files, logs get copied,...")
	rootCmd.Flags().DurationVarP(&testTimeout, "testtime", "", 5*time.Minute, "Timeout for a single test execution in a VM")
	rootCmd.Flags().BoolVarP(&version, "version", "", false, "Print version and exit")

	return rootCmd
}

func provisionAndExec(testRun *TestRun, startVM int) int {
	nrPool := make(chan int, testRun.nrVMs)
	for i := 0; i < testRun.nrVMs; i++ {
		nr := i + startVM
		nrPool <- nr

		lockName := fmt.Sprintf("%s.vm-%d.lock", testRun.cmdName, nr)
		lock, err := lockfile.New(filepath.Join(os.TempDir(), lockName))
		if err != nil {
			log.Fatalf("Cannot init lock. reason: %v", err)
		}
		if err = lock.TryLock(); err != nil {
			log.Fatalf("Cannot lock %q, reason: %v", lock, err)
		}
		defer lock.Unlock()
	}

	if testRun.vmSpec.ProvisionFile != "" {
		defer removeImages(testRun.vmSpec)
		if err := provisionImages(testRun.vmSpec, startVM); err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				log.Print(string(exitErr.Stderr))
			}
			log.Fatal(err)
		}
	}

	nFailed, err := execTests(testRun, nrPool)
	if err != nil {
		log.Printf("ERROR: %v", err)
	}
	return nFailed
}

func finalVMs(vmSpec *vmSpecification, to testOption, origTestnodes ...vmInstance) ([]vmInstance, error) {
	testnodes := origTestnodes

	if to.needsSameVMs {
		for i := range testnodes {
			testnodes[i].ImageName = testnodes[0].ImageName
		}
	} else if to.needsAllPlatforms { // this only includes the nodes under test
		oneVM := vmSpec.VMs[to.platformIdx]
		for i := range testnodes {
			testnodes[i].ImageName = vmSpec.ImageName(oneVM)
		}
	}

	return testnodes, nil
}

func ctxCancled(ctx context.Context) bool {
	return ctx.Err() != nil
}

func joinIfRel(basepath string, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(basepath, path)
}
