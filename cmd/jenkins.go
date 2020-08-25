package cmd

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
)

type Jenkins struct {
	wsPath string
}

func checkWorkspacePath(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("'%s' is not an absolute path", path)
	}
	if st, err := os.Stat(path); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("Could not stat %s: %v", path, err)
		}
		err := os.MkdirAll(path, 0755)
		if err != nil {
			return fmt.Errorf("Could not mkdir %s: %v", path, err)
		}
	} else if !st.IsDir() {
		return fmt.Errorf("'%s' is not a directory", path)
	}

	return nil
}

// NewJenkins initializes a new Jenkins object and assiciates a Jenkins workspace with the object.
// An empty workspace string disables other functionality.
func NewJenkins(workspacePath string) (*Jenkins, error) {
	if workspacePath != "" {
		if err := checkWorkspacePath(workspacePath); err != nil {
			return nil, err
		}
	}

	return &Jenkins{wsPath: workspacePath}, nil
}

// IsActive returnes true if a workspace path is set, otherwise false.
func (j *Jenkins) Workspace() string { return j.wsPath }
func (j *Jenkins) IsActive() bool    { return j.Workspace() != "" }

func (j *Jenkins) SubDir(subdir string) string {
	return filepath.Join(j.wsPath, subdir)
}

func (j *Jenkins) createSubDir(subdir string) (string, error) {
	if !j.IsActive() {
		return "", errors.New("This is not a jenkins run")
	}
	p := j.SubDir(subdir)

	if st, err := os.Stat(p); err == nil && st.IsDir() {
		return p, nil
	}

	return p, os.MkdirAll(p, 0755)
}

func (j *Jenkins) LogDir(testIDString string) string {
	return filepath.Join(j.Workspace(), "log", testIDString)
}

func (j *Jenkins) CreateFile(subDir string, name string) (*os.File, error) {
	p, err := j.createSubDir(subDir)
	if err != nil {
		return nil, err
	}

	return os.Create(filepath.Join(p, name))
}

// Log writes an arbitrary log file, where "subDir" is a subdirectory in the Jenkins workspace, and "name" is the name of the file to write to.
func (j *Jenkins) Log(subDir, name string, testLog []byte) error {
	p, err := j.createSubDir(subDir)
	if err != nil {
		return err
	}

	return ioutil.WriteFile(filepath.Join(p, name), testLog, 0644)
}

func (j *Jenkins) XMLLog(restultsDir, testName string, testRes TestResulter, testLog []byte) error {
	// Used to remove invalid runes from the output.
	re, err := regexp.Compile("[^\t\n\r\x20-\x7e]")
	if err != nil {
		return err
	}

	p, err := j.createSubDir(restultsDir)

	f, err := os.Create(filepath.Join(p, testName+".xml"))
	if err != nil {
		return err
	}
	defer f.Close()

	var nrFailed int
	if testRes.Err() != nil {
		nrFailed = 1 // currently there is only one test per execution
	}
	// header := fmt.Sprintf("<?xml version=\"1.0\" encoding=\"UTF-8\"?><testsuite tests=\"1\" failures=\"0\" errors=\"%d\">\n", status)
	header := fmt.Sprintf("<testsuite tests=\"1\" failures=\"%d\" assertions=\"1\">\n", nrFailed)
	header += fmt.Sprintf("<testcase classname=\"test.%s\" name=\"%s.run\" time=\"%.2f\">", testName, testName, testRes.ExecTime().Seconds())
	header += "<system-out>\n<![CDATA[\n"
	f.WriteString(header)
	f.Write(re.ReplaceAllLiteral(testLog, []byte{' '}))
	f.WriteString("]]></system-out>\n")
	if nrFailed > 0 {
		f.WriteString("<failure message=\"FAILED\"/>\n")
	}
	f.WriteString("</testcase></testsuite>")

	return nil
}
