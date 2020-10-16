package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

func XMLLog(resultsDir, testName string, testRes TestResulter, testLog []byte) error {
	// Used to remove invalid runes from the output.
	re, err := regexp.Compile("[^\t\n\r\x20-\x7e]")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(resultsDir, 0755); err != nil {
		return err
	}

	f, err := os.Create(filepath.Join(resultsDir, testName+".xml"))
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
		f.WriteString("<failure message=\"" + testRes.Err().Error() + "\">\n")
		f.WriteString("<![CDATA[\n")
		f.Write(re.ReplaceAllLiteral(testLog, []byte{' '}))
		f.WriteString("]]>\n</failure>\n")
	}
	f.WriteString("</testcase></testsuite>")

	return nil
}
