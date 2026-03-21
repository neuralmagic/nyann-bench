package warmup

import (
	"fmt"
	"io"
)

// PrintProfileSummary writes a human-readable profile summary.
func PrintProfileSummary(w io.Writer, p *Profile) {
	fmt.Fprintf(w, "\n=== Engine Profile ===\n")
	fmt.Fprintf(w, "Model:           %s\n", p.Model)
	fmt.Fprintf(w, "Workload:        ISL=%d, OSL=%d\n", p.WorkloadISL, p.WorkloadOSL)
	fmt.Fprintf(w, "Kernel warmup:   %d requests\n", p.KernelWarmupRequests)
	fmt.Fprintf(w, "TTFT:            %.1fms (p90)\n", p.TTFTMs)

	m := p.TPOTModel
	fmt.Fprintf(w, "\nTPOT model: %.2f + %.4f*C + %.6f*C^2  (ms)\n", m.A, m.B, m.C)
	fmt.Fprintf(w, "  Predicted TPOT at C=%d: %.1fms\n",
		p.TargetConcurrency,
		m.Predict(float64(p.TargetConcurrency)))

	d := p.Derived
	fmt.Fprintf(w, "\nDerived metrics:\n")
	if d.OptimalConcurrency > 0 {
		fmt.Fprintf(w, "  Optimal concurrency:  %d  (sqrt(a/c), max throughput)\n", d.OptimalConcurrency)
		fmt.Fprintf(w, "  Max throughput:        %.0f tok/s\n", d.MaxThroughputTokS)
	}
	if d.TwoxDegradationC > 0 {
		fmt.Fprintf(w, "  2x degradation:       C=%d\n", d.TwoxDegradationC)
	}
	fmt.Fprintf(w, "  Regime at C=%d:       %s\n", p.TargetConcurrency, d.Regime)

	if len(p.Levels) > 0 {
		fmt.Fprintf(w, "\nRaw measurements:\n")
		fmt.Fprintf(w, "  %-8s %-14s %-14s %s\n", "C", "TTFT(ms)", "TPOT(ms)", "predicted")
		for _, l := range p.Levels {
			fmt.Fprintf(w, "  %-8d p50=%-9.1f p50=%-9.2f %.2f\n",
				l.Concurrency, l.TTFTMs.P50, l.TPOTMs.P50,
				m.Predict(float64(l.Concurrency)))
		}
	}
	fmt.Fprintln(w)
}
