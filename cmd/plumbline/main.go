// Command plumbline asks questions over a document folder and evaluates the
// pipeline against a fixed question set.
//
//	plumbline ask   -corpus DIR [flags] "question"
//	plumbline eval  -corpus DIR -cases FILE [flags]
//	plumbline stats -corpus DIR
//
// Exit codes: 0 success, 1 evaluation gate failed, 2 usage or setup error.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/patlar104/plumbline/internal/eval"
	"github.com/patlar104/plumbline/internal/llm"
	"github.com/patlar104/plumbline/internal/rag"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `usage:
  plumbline ask   -corpus DIR [flags] "question"
  plumbline eval  -corpus DIR -cases FILE [flags]
  plumbline stats -corpus DIR
  plumbline version
Run "plumbline <command> -h" for flags.
Set ANTHROPIC_API_KEY to use Claude for routing, grading and generation;
without it every stage runs on its offline stand-in.`

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	var err error
	code := 0
	switch args[0] {
	case "ask":
		err = cmdAsk(ctx, args[1:], stdout, stderr)
	case "eval":
		code, err = cmdEval(ctx, args[1:], stdout, stderr)
	case "stats":
		err = cmdStats(args[1:], stdout, stderr)
	case "version", "-version", "--version":
		fmt.Fprintln(stdout, versionString())
		return 0
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, usage)
		return 0
	default:
		err = fmt.Errorf("unknown command %q\n%s", args[0], usage)
	}
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stderr, "plumbline:", err)
		}
		return 2
	}
	return code
}

// common holds the flags shared by ask and eval.
type common struct {
	corpus, web, allow, deny, model string
	offline                         bool
	maxWords                        int
	cfg                             rag.Config
}

func addCommon(fs *flag.FlagSet, c *common) {
	c.cfg = rag.DefaultConfig()
	fs.StringVar(&c.corpus, "corpus", "", "folder of .md/.txt documents (required)")
	fs.StringVar(&c.web, "web", "", "JSON file of {url,title,text} pages used as an offline web searcher")
	fs.StringVar(&c.allow, "allow-domains", "", "comma-separated trusted web domains")
	fs.StringVar(&c.deny, "deny-domains", "", "comma-separated web domains never used")
	fs.StringVar(&c.model, "model", "", "Claude model name (default $PLUMBLINE_MODEL or "+llm.DefaultModel+")")
	fs.BoolVar(&c.offline, "offline", false, "ignore ANTHROPIC_API_KEY and use offline stand-ins")
	fs.IntVar(&c.maxWords, "chunk-words", 120, "maximum words per chunk")
	fs.Float64Var(&c.cfg.Upper, "upper", c.cfg.Upper, "grader threshold for 'correct'")
	fs.Float64Var(&c.cfg.Lower, "lower", c.cfg.Lower, "grader threshold below which every passage is 'incorrect'")
	fs.IntVar(&c.cfg.MaxIter, "max-iter", c.cfg.MaxIter, "retry loop cap")
	fs.IntVar(&c.cfg.MaxLLM, "max-llm", c.cfg.MaxLLM, "model calls per question")
	fs.IntVar(&c.cfg.MaxWeb, "max-web", c.cfg.MaxWeb, "web searches per question")
	fs.DurationVar(&c.cfg.Deadline, "deadline", c.cfg.Deadline, "wall-clock limit per question")
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// setup loads the corpus and wires the pipeline factory.
func (c *common) setup(stderr io.Writer) (*rag.Index, func(rag.Config) (*rag.Pipeline, error), error) {
	if c.corpus == "" {
		return nil, nil, errors.New("-corpus is required")
	}
	if c.maxWords < 20 {
		return nil, nil, errors.New("-chunk-words must be at least 20")
	}
	docs, err := rag.LoadDir(c.corpus)
	if err != nil {
		return nil, nil, err
	}
	ix := rag.Build(docs, rag.HashEmbedder{}, c.maxWords)
	for _, w := range ix.Warnings {
		fmt.Fprintln(stderr, "index warning:", w)
	}
	opt := rag.Options{Trust: rag.TrustPolicy{Allow: splitList(c.allow), Deny: splitList(c.deny)}}
	if c.web != "" {
		w, err := rag.LoadStaticWeb(c.web)
		if err != nil {
			return nil, nil, err
		}
		opt.Web = w
	}
	if m, ok := llm.FromEnv(); ok && !c.offline {
		if c.model != "" {
			m.Model = c.model
		}
		opt.Model = m
		fmt.Fprintf(stderr, "mode: Claude (%s) with offline fallbacks\n", m.Model)
	} else {
		fmt.Fprintln(stderr, "mode: offline stand-ins (set ANTHROPIC_API_KEY to use Claude)")
	}
	build := func(cfg rag.Config) (*rag.Pipeline, error) { return rag.NewPipeline(ix, cfg, opt) }
	return ix, build, nil
}

func cmdAsk(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var c common
	addCommon(fs, &c)
	groups := fs.String("groups", "", "comma-separated groups the user belongs to")
	maxAge := fs.Int("max-age-days", 0, "ignore documents older than this many days (0 = no limit)")
	archived := fs.Bool("include-superseded", false, "also search documents that a newer one replaces")
	trace := fs.Bool("trace", false, "print the per-stage trace")
	asJSON := fs.Bool("json", false, "print the full result as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	question := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if question == "" {
		return errors.New("ask needs a question")
	}
	if *maxAge < 0 {
		return errors.New("-max-age-days cannot be negative")
	}
	_, build, err := c.setup(stderr)
	if err != nil {
		return err
	}
	p, err := build(c.cfg)
	if err != nil {
		return err
	}
	f := rag.Filter{Groups: splitList(*groups), MaxAge: time.Duration(*maxAge) * 24 * time.Hour, IncludeSuperseded: *archived}
	res, err := p.Ask(ctx, question, f)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	fmt.Fprintln(stdout, res.Answer)
	if len(res.Evidence) > 0 && !res.Abstained {
		fmt.Fprintln(stdout, "\nSources:")
		for _, e := range res.Evidence {
			fmt.Fprintf(stdout, "  [%d] %s (%s, %s)\n", e.N, e.Title, e.Source, e.Origin)
		}
	}
	fmt.Fprintf(stdout, "\nroute=%s grade=%s action=%s verified=%t iterations=%d model_calls=%d web=%d\n",
		res.Route.Kind, orDash(string(res.Grade)), orDash(res.Action), res.Verified, res.Iterations, res.LLMCalls, res.WebFetches)
	for _, w := range res.Warnings {
		fmt.Fprintln(stderr, "warning:", w)
	}
	if *trace {
		fmt.Fprintln(stdout, "\nTrace:")
		for _, e := range res.Trace {
			fmt.Fprintf(stdout, "  %-9s %7.2fms  %s\n", e.Step, e.DurationMS, e.Detail)
		}
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func cmdEval(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var c common
	addCommon(fs, &c)
	casesPath := fs.String("cases", "", "JSON Lines file of test cases (required)")
	sweep := fs.Bool("sweep", false, "also sweep the grader thresholds and report the best pair")
	noGate := fs.Bool("no-gate", false, "report only; never exit 1 on missed floors")
	asJSON := fs.Bool("json", false, "print the full report as JSON")
	verbose := fs.Bool("v", false, "print every case, not only failures")
	if err := fs.Parse(args); err != nil {
		return 0, err
	}
	if *casesPath == "" {
		return 0, errors.New("-cases is required")
	}
	cases, err := eval.LoadCases(*casesPath)
	if err != nil {
		return 0, err
	}
	_, build, err := c.setup(stderr)
	if err != nil {
		return 0, err
	}
	p, err := build(c.cfg)
	if err != nil {
		return 0, err
	}
	now := time.Now()
	rep, err := eval.Run(ctx, p, cases, now)
	if err != nil {
		return 0, err
	}
	violations := rep.Check(eval.DefaultFloors())
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return 0, err
		}
	} else {
		if err := printReport(stdout, rep, *verbose); err != nil {
			return 0, err
		}
	}
	if *sweep {
		rows, err := eval.Sweep(ctx, func(u, l float64) (*rag.Pipeline, error) {
			cfg := c.cfg
			cfg.Upper, cfg.Lower = u, l
			return build(cfg)
		}, cases, now, []float64{0.5, 0.6, 0.7, 0.8}, []float64{0.1, 0.2, 0.3, 0.4})
		if err != nil {
			return 0, err
		}
		fmt.Fprintln(stdout, "\nThreshold sweep (best first):")
		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  upper\tlower\tscore\trecall\tcoverage\tabstain\tgrounded\tleak")
		for _, r := range rows {
			m := r.Metrics
			fmt.Fprintf(tw, "  %.1f\t%.1f\t%.3f\t%.2f\t%.2f\t%.2f\t%.2f\t%.2f\n", r.Upper, r.Lower, r.Score, m.Recall, m.Coverage, m.AbstainAcc, m.Grounded, m.LeakRate)
		}
		if err := tw.Flush(); err != nil {
			return 0, fmt.Errorf("write sweep table: %w", err)
		}
	}
	if len(violations) > 0 {
		fmt.Fprintln(stderr, "\nGate FAILED:")
		for _, v := range violations {
			fmt.Fprintln(stderr, "  -", v)
		}
		if !*noGate {
			return 1, nil
		}
	} else {
		fmt.Fprintln(stderr, "\nGate passed.")
	}
	return 0, nil
}

func printReport(w io.Writer, rep eval.Report, verbose bool) error {
	m := rep.Overall
	fmt.Fprintf(w, "Cases: %d counted, %d passed\n", m.Cases, m.Passed)
	fmt.Fprintf(w, "recall %.2f  mrr %.2f  coverage %.2f  abstention %.2f  groundedness %.2f  leak %.2f  route %.2f\n",
		m.Recall, m.MRR, m.Coverage, m.AbstainAcc, m.Grounded, m.LeakRate, m.RouteAcc)
	cats := make([]string, 0, len(rep.ByCategory))
	for k := range rep.ByCategory {
		cats = append(cats, k)
	}
	sort.Strings(cats)
	fmt.Fprintln(w, "\nBy category:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, k := range cats {
		c := rep.ByCategory[k]
		fmt.Fprintf(tw, "  %s\t%d/%d passed\n", k, c.Passed, c.Cases)
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	fmt.Fprintln(w)
	for _, cr := range rep.Cases {
		status := "PASS"
		switch {
		case cr.XFail != "" && cr.Pass:
			status = "XPASS"
		case cr.XFail != "":
			status = "XFAIL"
		case !cr.Pass:
			status = "FAIL"
		}
		if status == "PASS" && !verbose {
			continue
		}
		fmt.Fprintf(w, "%-5s %s [%s]\n", status, cr.ID, cr.Category)
		for _, r := range cr.Reasons {
			fmt.Fprintf(w, "      - %s\n", r)
		}
		if cr.XFail != "" {
			fmt.Fprintf(w, "      expected failure: %s\n", cr.XFail)
		}
		if status != "PASS" {
			fmt.Fprintf(w, "      answer: %s\n", oneLine(cr.Result.Answer, 160))
		}
	}
	if len(rep.UnexpectedPasses) > 0 {
		fmt.Fprintf(w, "\nNow passing, remove their xfail: %s\n", strings.Join(rep.UnexpectedPasses, ", "))
	}
	return nil
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func cmdStats(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	fs.SetOutput(stderr)
	corpus := fs.String("corpus", "", "folder of .md/.txt documents (required)")
	words := fs.Int("chunk-words", 120, "maximum words per chunk")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *corpus == "" {
		return errors.New("-corpus is required")
	}
	docs, err := rag.LoadDir(*corpus)
	if err != nil {
		return err
	}
	ix := rag.Build(docs, rag.HashEmbedder{}, *words)
	flagged, superseded := 0, 0
	for _, ch := range ix.Chunks {
		if len(ch.Removed) > 0 {
			flagged++
		}
		if ch.Superseded {
			superseded++
		}
	}
	fmt.Fprintf(stdout, "documents %d\nchunks %d\nsummary nodes %d\nchunks with screened-out text %d\nsuperseded chunks %d\n",
		len(docs), len(ix.Chunks), len(ix.Summaries), flagged, superseded)
	for _, w := range ix.Warnings {
		fmt.Fprintln(stdout, "warning:", w)
	}
	return nil
}
