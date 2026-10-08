package collector

import (
	"regexp"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
)

// client_golang keeps fqName and help private and offers String(), which
// renders both with %q. So both are read back out of that, and the help has to
// be unquoted rather than taken as it appears: a help string containing a
// quotation mark arrives with the escapes still in it.
var (
	helpPattern   = regexp.MustCompile(`help: ("(?:[^"\\]|\\.)*")`)
	fqNamePattern = regexp.MustCompile(`fqName: "([^"]+)"`)
)

// helpOf reads a descriptor's help text back out of its String form.
//
// The alternative is writing every help string twice, once for the descriptor
// and once for whatever wants to check it, and two copies of a sentence is how
// one of them ends up describing something the metric no longer does.
func helpOf(d *prometheus.Desc) string {
	m := helpPattern.FindStringSubmatch(d.String())
	if len(m) != 2 {
		return ""
	}
	help, err := strconv.Unquote(m[1])
	if err != nil {
		return ""
	}
	return help
}

// fqNameOf reads a descriptor's metric name back out of its String form.
func fqNameOf(d *prometheus.Desc) string {
	m := fqNamePattern.FindStringSubmatch(d.String())
	if len(m) != 2 {
		return ""
	}
	return m[1]
}
