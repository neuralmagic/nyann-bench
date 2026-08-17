package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/neuralmagic/nyann-bench/pkg/config"
)

func TestCompileStarlarkCommandEmitsBoundedScenarioIR(t *testing.T) {
	cmd := compileStarlarkCmd()
	cmd.SetIn(bytes.NewBufferString(`scenario(stages=[stage("1s", concurrency=3)])`))
	var output bytes.Buffer
	cmd.SetOut(&output)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var scenario config.ScenarioConfig
	if err := json.Unmarshal(output.Bytes(), &scenario); err != nil {
		t.Fatal(err)
	}
	if len(scenario.Stages) != 1 || scenario.Stages[0].Concurrency != 3 {
		t.Fatalf("compiled scenario = %+v", scenario)
	}
}
