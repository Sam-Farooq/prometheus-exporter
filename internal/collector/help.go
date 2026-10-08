package collector

import (
	"regexp"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	helpPattern   = regexp.MustCompile(`help: "((?:[^"\\]|\\.)*)"`)
	fqNamePattern = regexp.MustCompile(`fqName: "([^"]+)"`)
)

// helpOf reads a descriptor's help text back out of its String form.
//
// client_golang keeps the help string private, and the alternative is writing
// each one twice. Two copies of a sentence is how one of them ends up
// describing something the metric no longer does.
func helpOf(d *prometheus.Desc) string {
	m := helpPattern.FindStringSubmatch(d.String())
	if len(m) != 2 {
		return ""
	}
	return m[1]
}

// fqNameOf reads a descriptor's metric name back out of its String form, for
// the same reason helpOf exists.
func fqNameOf(d *prometheus.Desc) string {
	m := fqNamePattern.FindStringSubmatch(d.String())
	if len(m) != 2 {
		return ""
	}
	return m[1]
}
