package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
)

// Starlark type names for values returned by builtins.
const (
	starlarkTypeWorkload = "Workload"
	starlarkTypeStage    = "Stage"
	starlarkTypeBarrier  = "Barrier"
)

// ParseStarlark evaluates a .star file and returns the ScenarioConfig
// registered by the script's scenario() call.
func ParseStarlark(path string) (*ScenarioConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	var result *ScenarioConfig
	var scenarioCalled bool

	builtins := starlark.StringDict{
		"workload": starlark.NewBuiltin("workload", builtinWorkload),
		"stage":    starlark.NewBuiltin("stage", builtinStage),
		"barrier":  starlark.NewBuiltin("barrier", builtinBarrier),
		"scenario": starlark.NewBuiltin("scenario", func(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			if scenarioCalled {
				return nil, fmt.Errorf("scenario() can only be called once")
			}
			scenarioCalled = true
			sc, err := parseScenarioCall(args, kwargs)
			if err != nil {
				return nil, err
			}
			result = sc
			return starlark.None, nil
		}),
	}

	thread := &starlark.Thread{Name: path}
	_, err = starlark.ExecFile(thread, path, data, builtins)
	if err != nil {
		return nil, fmt.Errorf("executing %s: %w", path, err)
	}

	if result == nil {
		return nil, fmt.Errorf("%s: no scenario() call found", path)
	}

	return result, nil
}

// builtinWorkload implements the workload() Starlark builtin.
func builtinWorkload(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var (
		typ            string
		isl            = 128
		osl            = 256
		turns          = 1
		subsequentISL  starlark.Value = starlark.None
		corpusPath     starlark.Value = starlark.None
		gsm8kPath      starlark.Value = starlark.None
		gsm8kTrainPath starlark.Value = starlark.None
		numFewshot     = 5
		charsPerToken  = 0.0
		cacheSalt      starlark.Value = starlark.None
		name           starlark.Value = starlark.None
	)

	if err := starlark.UnpackArgs("workload", args, kwargs,
		"type", &typ,
		"isl?", &isl,
		"osl?", &osl,
		"turns?", &turns,
		"subsequent_isl?", &subsequentISL,
		"corpus_path?", &corpusPath,
		"gsm8k_path?", &gsm8kPath,
		"gsm8k_train_path?", &gsm8kTrainPath,
		"num_fewshot?", &numFewshot,
		"chars_per_token?", &charsPerToken,
		"cache_salt?", &cacheSalt,
		"name?", &name,
	); err != nil {
		return nil, err
	}

	// Validate type
	switch typ {
	case "synthetic", "faker", "corpus", "gsm8k":
	default:
		return nil, fmt.Errorf("unknown workload type %q (options: synthetic, faker, corpus, gsm8k)", typ)
	}

	// Validate type-specific requirements
	if typ == "corpus" && (corpusPath == starlark.None || starlarkString(corpusPath) == "") {
		return nil, fmt.Errorf("corpus_path is required when type is \"corpus\"")
	}
	if typ == "gsm8k" && (gsm8kPath == starlark.None || starlarkString(gsm8kPath) == "") {
		return nil, fmt.Errorf("gsm8k_path is required when type is \"gsm8k\"")
	}
	if typ == "gsm8k" && numFewshot > 0 && (gsm8kTrainPath == starlark.None || starlarkString(gsm8kTrainPath) == "") {
		return nil, fmt.Errorf("gsm8k_train_path is required when num_fewshot > 0")
	}

	return starlarkstruct.FromStringDict(starlark.String(starlarkTypeWorkload), starlark.StringDict{
		"type":            starlark.String(typ),
		"isl":             starlark.MakeInt(isl),
		"osl":             starlark.MakeInt(osl),
		"turns":           starlark.MakeInt(turns),
		"subsequent_isl":  subsequentISL,
		"corpus_path":     corpusPath,
		"gsm8k_path":      gsm8kPath,
		"gsm8k_train_path": gsm8kTrainPath,
		"num_fewshot":     starlark.MakeInt(numFewshot),
		"chars_per_token": starlark.Float(charsPerToken),
		"cache_salt":      cacheSalt,
		"name":            name,
	}), nil
}

// builtinStage implements the stage() Starlark builtin.
func builtinStage(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var (
		durationVal starlark.Value
		concurrency = 10
		rate        starlark.Value = starlark.None
		mode        = "concurrent"
		maxInFlight = 0
		maxRequests = 0
		rampup      starlark.Value = starlark.None
		workload    starlark.Value = starlark.None
		target      starlark.Value = starlark.None
		model       starlark.Value = starlark.None
		name        starlark.Value = starlark.None
		warmup      = false
	)

	if err := starlark.UnpackArgs("stage", args, kwargs,
		"duration", &durationVal,
		"concurrency?", &concurrency,
		"rate?", &rate,
		"mode?", &mode,
		"max_inflight?", &maxInFlight,
		"max_requests?", &maxRequests,
		"rampup?", &rampup,
		"workload?", &workload,
		"target?", &target,
		"model?", &model,
		"name?", &name,
		"warmup?", &warmup,
	); err != nil {
		return nil, err
	}

	// Validate duration
	dur, err := parseDurationValue(durationVal)
	if err != nil {
		return nil, fmt.Errorf("stage duration: %w", err)
	}

	// Validate mode
	switch mode {
	case "concurrent", "constant", "poisson":
	default:
		return nil, fmt.Errorf("unknown mode %q (options: concurrent, constant, poisson)", mode)
	}

	// Validate mutual exclusivity: if rate is set, concurrency must be default
	if rate != starlark.None {
		// User explicitly set rate; check they didn't also explicitly set concurrency.
		// We can detect this by checking if concurrency was provided via kwargs.
		for _, kv := range kwargs {
			if string(kv[0].(starlark.String)) == "concurrency" {
				return nil, fmt.Errorf("concurrency and rate are mutually exclusive")
			}
		}
	}

	if concurrency < 1 {
		return nil, fmt.Errorf("concurrency must be >= 1, got %d", concurrency)
	}

	// Validate workload type if provided
	if workload != starlark.None {
		if s, ok := workload.(*starlarkstruct.Struct); ok {
			if s.Constructor() != starlark.String(starlarkTypeWorkload) {
				return nil, fmt.Errorf("workload: expected Workload, got %s", s.Constructor())
			}
		} else {
			return nil, fmt.Errorf("workload: expected Workload, got %s", workload.Type())
		}
	}

	return starlarkstruct.FromStringDict(starlark.String(starlarkTypeStage), starlark.StringDict{
		"duration":     starlark.String(dur.String()),
		"concurrency":  starlark.MakeInt(concurrency),
		"rate":         rate,
		"mode":         starlark.String(mode),
		"max_inflight": starlark.MakeInt(maxInFlight),
		"max_requests": starlark.MakeInt(maxRequests),
		"rampup":       rampup,
		"workload":     workload,
		"target":       target,
		"model":        model,
		"name":         name,
		"warmup":       starlark.Bool(warmup),
	}), nil
}

// builtinBarrier implements the barrier() Starlark builtin.
// barrier() marks a synchronization point in the stage list.
// barrier(drain=True) stops the pool before syncing.
func builtinBarrier(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var drain bool

	if err := starlark.UnpackArgs("barrier", args, kwargs,
		"drain?", &drain,
	); err != nil {
		return nil, err
	}

	return starlarkstruct.FromStringDict(starlark.String(starlarkTypeBarrier), starlark.StringDict{
		"drain": starlark.Bool(drain),
	}), nil
}

// parseScenarioCall processes the arguments to scenario().
func parseScenarioCall(args starlark.Tuple, kwargs []starlark.Tuple) (*ScenarioConfig, error) {
	var (
		stagesList *starlark.List
		target     starlark.Value = starlark.None
		model      starlark.Value = starlark.None
		workload   starlark.Value = starlark.None
	)

	if err := starlark.UnpackArgs("scenario", args, kwargs,
		"stages", &stagesList,
		"target?", &target,
		"model?", &model,
		"workload?", &workload,
	); err != nil {
		return nil, err
	}

	if stagesList.Len() == 0 {
		return nil, fmt.Errorf("stages must contain at least one stage")
	}

	sc := &ScenarioConfig{
		Target: starlarkString(target),
		Model:  starlarkString(model),
	}

	// Parse default workload
	if workload != starlark.None {
		s, ok := workload.(*starlarkstruct.Struct)
		if !ok || s.Constructor() != starlark.String(starlarkTypeWorkload) {
			return nil, fmt.Errorf("workload: expected Workload, got %s", workload.Type())
		}
		w, err := structToWorkload(s)
		if err != nil {
			return nil, fmt.Errorf("workload: %w", err)
		}
		sc.Workload = *w
	} else {
		// Default workload matching JSON defaults
		sc.Workload = Workload{
			Type:  "faker",
			ISL:   128,
			OSL:   256,
			Turns: 1,
		}
	}

	// Parse stages (accepts both Stage and Barrier structs)
	iter := stagesList.Iterate()
	defer iter.Done()
	var val starlark.Value
	i := 0
	for iter.Next(&val) {
		s, ok := val.(*starlarkstruct.Struct)
		if !ok {
			return nil, fmt.Errorf("stages[%d]: expected Stage or barrier(), got %s", i, val.Type())
		}

		switch s.Constructor() {
		case starlark.String(starlarkTypeStage):
			stage, err := structToScenarioStage(s)
			if err != nil {
				return nil, fmt.Errorf("stages[%d]: %w", i, err)
			}
			sc.Stages = append(sc.Stages, *stage)

		case starlark.String(starlarkTypeBarrier):
			drainVal, _ := s.Attr("drain")
			drain := false
			if drainVal != nil && drainVal != starlark.None {
				drain = bool(drainVal.(starlark.Bool))
			}
			sc.Stages = append(sc.Stages, ScenarioStage{
				Barrier:      true,
				BarrierDrain: drain,
			})

		default:
			return nil, fmt.Errorf("stages[%d]: expected Stage or barrier(), got %s", i, s.Constructor())
		}
		i++
	}

	return sc, nil
}

// structToWorkload converts a Starlark Workload struct to a Go Workload.
func structToWorkload(s *starlarkstruct.Struct) (*Workload, error) {
	w := &Workload{}

	typ, _ := s.Attr("type")
	w.Type = starlarkString(typ)

	isl, _ := s.Attr("isl")
	w.ISL = starlarkInt(isl)

	osl, _ := s.Attr("osl")
	w.OSL = starlarkInt(osl)

	turns, _ := s.Attr("turns")
	w.Turns = starlarkInt(turns)

	subISL, _ := s.Attr("subsequent_isl")
	if subISL != starlark.None {
		v := starlarkInt(subISL)
		w.SubsequentISL = &v
	}

	corpusPath, _ := s.Attr("corpus_path")
	w.CorpusPath = starlarkString(corpusPath)

	gsm8kPath, _ := s.Attr("gsm8k_path")
	w.GSM8KPath = starlarkString(gsm8kPath)

	gsm8kTrainPath, _ := s.Attr("gsm8k_train_path")
	w.GSM8KTrainPath = starlarkString(gsm8kTrainPath)

	numFewshot, _ := s.Attr("num_fewshot")
	if numFewshot != starlark.None {
		v := starlarkInt(numFewshot)
		w.NumFewShot = &v
	}

	cpt, _ := s.Attr("chars_per_token")
	w.CharsPerToken = starlarkFloat(cpt)

	cacheSalt, _ := s.Attr("cache_salt")
	if cacheSalt != starlark.None {
		saltStr := starlarkString(cacheSalt)
		w.CacheSalt = parseCacheSaltString(saltStr)
	}

	name, _ := s.Attr("name")
	w.Name = starlarkString(name)

	return w, nil
}

// structToScenarioStage converts a Starlark Stage struct to a Go ScenarioStage.
func structToScenarioStage(s *starlarkstruct.Struct) (*ScenarioStage, error) {
	stage := &ScenarioStage{}

	durStr, _ := s.Attr("duration")
	dur, err := time.ParseDuration(starlarkString(durStr))
	if err != nil {
		return nil, fmt.Errorf("duration: %w", err)
	}
	stage.Duration = dur

	conc, _ := s.Attr("concurrency")
	stage.Concurrency = starlarkInt(conc)

	rate, _ := s.Attr("rate")
	if rate != starlark.None {
		stage.Rate = starlarkFloat(rate)
	}

	mode, _ := s.Attr("mode")
	stage.Mode = starlarkString(mode)

	maxInFlight, _ := s.Attr("max_inflight")
	stage.MaxInFlight = starlarkInt(maxInFlight)

	maxReqs, _ := s.Attr("max_requests")
	stage.MaxRequests = starlarkInt(maxReqs)

	rampup, _ := s.Attr("rampup")
	if rampup != starlark.None {
		r, err := parseDurationValue(rampup)
		if err != nil {
			return nil, fmt.Errorf("rampup: %w", err)
		}
		stage.Rampup = r
	}

	workload, _ := s.Attr("workload")
	if workload != starlark.None {
		ws, ok := workload.(*starlarkstruct.Struct)
		if !ok {
			return nil, fmt.Errorf("workload: expected Workload struct")
		}
		w, err := structToWorkload(ws)
		if err != nil {
			return nil, fmt.Errorf("workload: %w", err)
		}
		stage.Workload = w
	}

	target, _ := s.Attr("target")
	stage.Target = starlarkString(target)

	model, _ := s.Attr("model")
	stage.Model = starlarkString(model)

	nameVal, _ := s.Attr("name")
	stage.Name = starlarkString(nameVal)

	warmup, _ := s.Attr("warmup")
	if warmup != starlark.None {
		stage.Warmup = bool(warmup.(starlark.Bool))
	}

	return stage, nil
}

// parseDurationValue parses a Starlark value as a Go duration.
// Accepts strings ("60s", "5m") or ints (seconds).
func parseDurationValue(v starlark.Value) (time.Duration, error) {
	switch val := v.(type) {
	case starlark.String:
		d, err := time.ParseDuration(string(val))
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q: %w", string(val), err)
		}
		return d, nil
	case starlark.Int:
		secs, ok := val.Int64()
		if !ok {
			return 0, fmt.Errorf("duration integer too large")
		}
		return time.Duration(secs) * time.Second, nil
	default:
		return 0, fmt.Errorf("duration must be a string (\"60s\") or int (seconds), got %s", v.Type())
	}
}

// parseCacheSaltString converts "random" or "fixed:VALUE" to a CacheSalt.
func parseCacheSaltString(s string) *CacheSalt {
	if s == "random" {
		return &CacheSalt{Mode: "random"}
	}
	if strings.HasPrefix(s, "fixed:") {
		return &CacheSalt{Mode: "fixed", Value: strings.TrimPrefix(s, "fixed:")}
	}
	return nil
}

// Helper functions for extracting Go values from Starlark values.

func starlarkString(v starlark.Value) string {
	if v == nil || v == starlark.None {
		return ""
	}
	if s, ok := v.(starlark.String); ok {
		return string(s)
	}
	return v.String()
}

func starlarkInt(v starlark.Value) int {
	if v == nil || v == starlark.None {
		return 0
	}
	if i, ok := v.(starlark.Int); ok {
		val, _ := i.Int64()
		return int(val)
	}
	return 0
}

func starlarkFloat(v starlark.Value) float64 {
	if v == nil || v == starlark.None {
		return 0
	}
	switch val := v.(type) {
	case starlark.Float:
		return float64(val)
	case starlark.Int:
		i, _ := val.Int64()
		return float64(i)
	}
	return 0
}
