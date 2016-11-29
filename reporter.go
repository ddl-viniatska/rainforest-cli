package main

import (
	"encoding/xml"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/rainforestapp/rainforest-cli/rainforest"
	"github.com/urfave/cli"
)

// resourceAPI is part of the API connected to available resources
type reporterAPI interface {
	GetRunTestDetails(int, int) (*rainforest.RunTestDetails, error)
}

type reporter struct {
	getRunDetails               func(int, *rainforest.Client) (*rainforest.RunDetails, error)
	createOutputFile            func(string) (*os.File, error)
	createJunitReportSchema     func(*rainforest.RunDetails, *rainforest.Client) (*jUnitReportSchema, error)
	createJunitTestReportSchema func(int, *[]rainforest.RunTestDetails, reporterAPI) (*[]jUnitTestReportSchema, error)
	writeJUnitReport            func(*jUnitReportSchema, *os.File) error
}

func createReport(c *cli.Context) error {
	r := newReporter()
	return r.createReport(c)
}

func newReporter() *reporter {
	return &reporter{
		getRunDetails:               getRunDetails,
		createOutputFile:            createOutputFile,
		createJunitReportSchema:     createJunitReportSchema,
		createJunitTestReportSchema: createJunitTestReportSchema,
		writeJUnitReport:            writeJUnitReport,
	}
}

func (r *reporter) createReport(c cliContext) error {
	var runID int
	var err error

	if runIDArg := c.Args().Get(0); runIDArg != "" {
		runID, err = strconv.Atoi(runIDArg)
		if err != nil {
			return err
		}
	} else if deprecatedRunIDArg := c.String("run-id"); deprecatedRunIDArg != "" {
		runID, err = strconv.Atoi(deprecatedRunIDArg)
		if err != nil {
			return err
		}

		log.Println("Warning: `run-id` flag is deprecated. Please provide Run ID as an argument.")
	} else {
		return cli.NewExitError("No run ID argument found.", 1)
	}

	if junitFile := c.String("junit-file"); junitFile != "" {
		err = r.createJUnitReport(runID, junitFile)
		if err != nil {
			log.Fatalf("Error creating JUnit Report: %v", err.Error())
			return err
		}
	}

	return nil
}

func (r *reporter) createJUnitReport(runID int, junitFile string) error {
	log.Print("Creating JUnit report for run #" + strconv.Itoa(runID) + ": " + junitFile)

	if filepath.Ext(junitFile) != ".xml" {
		return fmt.Errorf("JUnit file extension must be .xml")
	}

	filepath, err := filepath.Abs(junitFile)
	if err != nil {
		return err
	}

	var runDetails *rainforest.RunDetails
	runDetails, err = r.getRunDetails(runID, api)
	if err != nil {
		return err
	}

	var outputFile *os.File
	outputFile, err = r.createOutputFile(filepath)
	if err != nil {
		return err
	}

	var reportSchema *jUnitReportSchema
	reportSchema, err = r.createJunitReportSchema(runDetails, api)
	if err != nil {
		return err
	}

	err = r.writeJUnitReport(reportSchema, outputFile)
	if err != nil {
		return err
	}

	return nil
}

func getRunDetails(runID int, client *rainforest.Client) (*rainforest.RunDetails, error) {
	var runDetails *rainforest.RunDetails
	var err error

	log.Printf("Fetching details for run #" + strconv.Itoa(runID))
	if runDetails, err = client.GetRunDetails(runID); err != nil {
		return runDetails, err
	}

	if !runDetails.StateDetails.IsFinalState {
		err = fmt.Errorf("Report cannot be created for an incomplete run")
	}

	return runDetails, err
}

type jUnitTestReportFailure struct {
	XMLName xml.Name `xml:"failure"`
	Type    string   `xml:"type,attr"`
	Message string   `xml:"message,attr"`
}

type jUnitTestReportSchema struct {
	XMLName  xml.Name `xml:"testcase"`
	Name     string   `xml:"name,attr"`
	Time     float64  `xml:"time,attr"`
	Failures []jUnitTestReportFailure
}

type jUnitReportSchema struct {
	XMLName   xml.Name `xml:"testsuite"`
	Name      string   `xml:"name,attr"`
	Tests     int      `xml:"tests,attr"`
	Errors    int      `xml:"errors,attr"`
	Failures  int      `xml:"failures,attr"`
	Time      float64  `xml:"time,attr"`
	TestCases []jUnitTestReportSchema
}

func createJunitReportSchema(runDetails *rainforest.RunDetails, client *rainforest.Client) (*jUnitReportSchema, error) {
	finalStateName := runDetails.StateDetails.Name
	duration, err := timeStringDifferenceSecs(runDetails.Timestamps["created_at"], runDetails.Timestamps[finalStateName])
	if err != nil {
		return &jUnitReportSchema{}, err
	}

	var testCases *[]jUnitTestReportSchema
	testCases, err = createJunitTestReportSchema(runDetails.ID, &runDetails.Tests, client)
	if err != nil {
		return &jUnitReportSchema{}, err
	}

	report := &jUnitReportSchema{
		Name:      runDetails.Description,
		Errors:    runDetails.TotalNoResultTests,
		Failures:  runDetails.TotalFailedTests,
		Tests:     runDetails.TotalTests,
		TestCases: *testCases,
		Time:      duration,
	}

	return report, nil
}

func createJunitTestReportSchema(runID int, tests *[]rainforest.RunTestDetails, api reporterAPI) (*[]jUnitTestReportSchema, error) {
	type processedTestCase struct {
		TestCase jUnitTestReportSchema
		Error    error
	}

	testChan := make(chan processedTestCase, len(*tests))

	// Limit the maximum concurrent requests
	httpThreads := make(chan struct{}, maxWebConcurrency)

	processTest := func(test rainforest.RunTestDetails) {
		testCase := jUnitTestReportSchema{}

		duration, err := timeStringDifferenceSecs(test.CreatedAt, test.UpdatedAt)

		if err != nil {
			testChan <- processedTestCase{TestCase: testCase, Error: err}
			return
		}

		testCase.Name = test.Title
		testCase.Time = duration

		if test.Result == "failed" {
			var testDetails *rainforest.RunTestDetails

			// reserve a thread
			httpThreads <- struct{}{}
			log.Printf("Fetching information for failed test #" + strconv.Itoa(test.ID))
			testDetails, err = api.GetRunTestDetails(runID, test.ID)
			// release the thread
			<-httpThreads

			if err != nil {
				testChan <- processedTestCase{TestCase: jUnitTestReportSchema{}, Error: err}
				return
			}

			for _, step := range testDetails.Steps {
				for _, browser := range step.Browsers {
					browserName := browser.Name

					for _, feedback := range browser.Feedback {
						if feedback.AnswerGiven == "no" && feedback.JobState == "approved" && feedback.Note != "" {
							reportFailure := jUnitTestReportFailure{Type: browserName, Message: feedback.Note}
							testCase.Failures = append(testCase.Failures, reportFailure)
						}
					}
				}
			}
		}

		testChan <- processedTestCase{TestCase: testCase}
	}

	for _, test := range *tests {
		go processTest(test)
	}

	testCases := []jUnitTestReportSchema{}

	for i := 0; i < len(*tests); i++ {
		processed := <-testChan

		if processed.Error != nil {
			return &[]jUnitTestReportSchema{}, processed.Error
		}

		testCases = append(testCases, processed.TestCase)
	}

	return &testCases, nil
}

func writeJUnitReport(reportSchema *jUnitReportSchema, file *os.File) error {
	enc := xml.NewEncoder(file)

	file.Write([]byte(xml.Header))

	enc.Indent("", "  ")
	err := enc.Encode(reportSchema)
	if err != nil {
		return err
	}

	log.Printf("JUnit report successfully written to %v", file.Name())

	file.Close()
	return nil
}

/*
	Utility Functions
*/

func createOutputFile(filepath string) (*os.File, error) {
	var file *os.File
	var err error
	if file, err = os.Create(filepath); err != nil {
		return file, err
	}
	return file, err
}

func timeStringDifferenceSecs(start string, end string) (float64, error) {
	var startTime time.Time
	var endTime time.Time
	var err error
	var diff float64

	if startTime, err = time.Parse(time.RFC3339Nano, start); err != nil {
		return diff, err
	}

	if endTime, err = time.Parse(time.RFC3339Nano, end); err != nil {
		return diff, err
	}

	diff = endTime.Sub(startTime).Seconds()

	return diff, nil
}
