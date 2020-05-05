package cmd

import (
	"context"
	"flag"
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

var vmSpec vmSpecification

type testSpecification struct {
	TestSuiteFile string      `toml:"test_suite_file"`
	TestGroups    []testGroup `toml:"group"`
}

var jenkins *Jenkins

var (
	cmdName = filepath.Base(os.Args[0])

	vmSpecPath   = flag.String("vms", "vms.toml", "File containing VM specification")
	testSpecPath = flag.String("tests", "tests.toml", "File containing test specification")
	toRun        = flag.String("torun", "all", "comma separated list of test names to execute ('all' is a reserved test name)")
	startVM      = flag.Int("startvm", 2, "Number of the first VM to start in parallel")
	nrVMs        = flag.Int("nvms", 12, "Maximum number of VMs to start in parallel, starting at -startvm")
	failTest     = flag.Bool("failtest", false, "Stop executing tests when the first one failed")
	failGrp      = flag.Bool("failgroup", false, "Stop executing tests when at least one failed in the test group")
	quiet        = flag.Bool("quiet", false, "Don't print progess messages while tests are running")
	jenkinsWS    = flag.String("jenkins", "", "If this is set to a path for the current job, text output is saved to files, logs get copied,...")
	testTimeout  = flag.Duration("testtime", 5*time.Minute, "Timeout for a single test execution in a VM")
	version      = flag.Bool("version", false, "Print version and exit")
)

// Execute runs vmshed
func Execute() {
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

	if _, err := toml.DecodeFile(*vmSpecPath, &vmSpec); err != nil {
		log.Fatal(err)
	}
	vmSpec.ProvisionFile = joinIfRel(filepath.Dir(*vmSpecPath), vmSpec.ProvisionFile)

	var testSpec testSpecification
	if _, err := toml.DecodeFile(*testSpecPath, &testSpec); err != nil {
		log.Fatal(err)
	}
	testSpec.TestSuiteFile = joinIfRel(filepath.Dir(*testSpecPath), testSpec.TestSuiteFile)

	var tests []testGroup
	for _, test := range testSpec.TestGroups {
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
	testSpec.TestGroups = tests

	start := time.Now()

	nFailed := provisionAndExec(testSpec)

	log.Println(cmdName, "OVERALL EXECUTIONTIME:", time.Since(start))

	// transfer ownership to Jenkins, so that the workspace can be cleaned before running again
	if err := jenkins.OwnWorkspace(); err != nil {
		log.Println(cmdName, "ERROR SETTING WORKSPACE OWNERSHIP:", err)
	}

	os.Exit(nFailed)
}

func provisionAndExec(testSpec testSpecification) int {
	nrPool := make(chan int, *nrVMs)
	for i := 0; i < *nrVMs; i++ {
		nr := i + *startVM
		nrPool <- nr

		lockName := fmt.Sprintf("%s.vm-%d.lock", cmdName, nr)
		lock, err := lockfile.New(filepath.Join(os.TempDir(), lockName))
		if err != nil {
			log.Fatalf("Cannot init lock. reason: %v", err)
		}
		if err = lock.TryLock(); err != nil {
			log.Fatalf("Cannot lock %q, reason: %v", lock, err)
		}
		defer lock.Unlock()
	}

	if vmSpec.ProvisionFile != "" {
		defer removeImages()
		if err := provisionImages(); err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				log.Print(string(exitErr.Stderr))
			}
			log.Fatal(err)
		}
	}

	nFailed, err := execTests(&testSpec, *nrVMs, nrPool)
	if err != nil {
		log.Println(cmdName, "ERROR:", err)
	}
	return nFailed
}

func finalVMs(to testOption, origTestnodes ...vmInstance) ([]vmInstance, error) {
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
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(basepath, path)
}
