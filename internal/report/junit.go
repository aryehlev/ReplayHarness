package report

import (
	"encoding/xml"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/aryehlev/ReplayHarness/internal/model"
)

type junitSuites struct {
	XMLName  xml.Name     `xml:"testsuites"`
	Name     string       `xml:"name,attr"`
	Tests    int          `xml:"tests,attr"`
	Failures int          `xml:"failures,attr"`
	Suites   []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name     string      `xml:"name,attr"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
	Cases    []junitCase `xml:"testcase"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	Classname string        `xml:"classname,attr"`
	Time      string        `xml:"time,attr"`
	Failure   *junitFailure `xml:"failure,omitempty"`
}

type junitFailure struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Text    string `xml:",chardata"`
}

// WriteJUnit writes one test case per scenario, grouped by endpoint. A case
// fails when it has a finding at or above failOn ("error" or "warning").
func WriteJUnit(path string, r *Report, failOn string) error {
	threshold := model.SevError.Rank()
	switch failOn {
	case "warning":
		threshold = model.SevWarning.Rank()
	case "never":
		threshold = 99
	}
	suites := map[string]*junitSuite{}
	var names []string
	all := junitSuites{Name: "replayharness"}
	for _, res := range r.Results {
		ep := res.Scenario.Endpoint
		s, ok := suites[ep]
		if !ok {
			s = &junitSuite{Name: ep}
			suites[ep] = s
			names = append(names, ep)
		}
		secs := 0.0
		for _, o := range res.Observed {
			secs += o.DurationMS / 1000
		}
		tc := junitCase{Name: clean(fmt.Sprintf("[%s] %s", res.Scenario.Category, res.Scenario.Name)), Classname: clean(ep), Time: fmt.Sprintf("%.3f", secs)}
		var failing []model.Finding
		for _, f := range res.Findings {
			if f.Severity.Rank() >= threshold {
				failing = append(failing, f)
			}
		}
		if len(failing) > 0 {
			var text strings.Builder
			for _, f := range failing {
				fmt.Fprintf(&text, "%s %s: %s\n", f.Severity, f.Check, f.Message)
				if f.Detail != "" {
					fmt.Fprintf(&text, "  %s\n", f.Detail)
				}
			}
			fmt.Fprintf(&text, "\n%s\n", Curl(Options{Target: r.Target}, res.Scenario))
			tc.Failure = &junitFailure{Message: clean(failing[0].Message), Type: failing[0].Check, Text: clean(text.String())}
			s.Failures++
			all.Failures++
		}
		s.Cases = append(s.Cases, tc)
		s.Tests++
		all.Tests++
	}
	sort.Strings(names)
	for _, n := range names {
		all.Suites = append(all.Suites, *suites[n])
	}
	data, err := xml.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append([]byte(xml.Header), append(data, '\n')...), 0o644)
}

// clean drops characters that XML 1.0 cannot carry.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' || (r >= 0x20 && r != 0xFFFE && r != 0xFFFF) {
			return r
		}
		return -1
	}, s)
}
