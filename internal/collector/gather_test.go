package collector

import (
	"bytes"
	"sort"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
)

// gatherInto renders a registry to the text exposition format, keeping only
// the named metrics and sorting them, so a golden file is stable.
func gatherInto(buf *bytes.Buffer, reg prometheus.Gatherer, names []string) error {
	keep := make(map[string]bool, len(names))
	for _, n := range names {
		keep[n] = true
	}

	families, err := reg.Gather()
	if err != nil {
		return err
	}
	wanted := make([]*dto.MetricFamily, 0, len(families))
	for _, mf := range families {
		if keep[mf.GetName()] {
			wanted = append(wanted, mf)
		}
	}
	sort.Slice(wanted, func(i, j int) bool { return wanted[i].GetName() < wanted[j].GetName() })

	enc := expfmt.NewEncoder(buf, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, mf := range wanted {
		if err := enc.Encode(mf); err != nil {
			return err
		}
	}
	return nil
}
