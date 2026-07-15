package batch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestRunChainOrderAndContinueOnNonCritical(t *testing.T) {
	var order []string
	step := func(name string, fail bool) Step {
		return Step{Name: name, Run: func(context.Context) (string, error) {
			order = append(order, name)
			if fail {
				return "", errors.New("boom")
			}
			return "ok", nil
		}}
	}
	res, err := RunChain(context.Background(),
		[]Step{step("a", false), step("b", true), step("c", false)}, nil, quiet())
	if err != nil {
		t.Fatalf("non-critical failure must not fail the chain: %v", err)
	}
	if len(order) != 3 || order[0] != "a" || order[1] != "b" || order[2] != "c" {
		t.Fatalf("steps ran out of order: %v", order)
	}
	if res.Failed != 1 || res.Aborted {
		t.Errorf("result = failed:%d aborted:%v, want 1/false", res.Failed, res.Aborted)
	}
	if res.Steps[1].Status != "failed" || res.Steps[2].Status != "done" {
		t.Errorf("statuses: %+v", res.Steps)
	}
}

func TestRunChainCriticalFailureAbortsAndSkips(t *testing.T) {
	ran := map[string]bool{}
	mk := func(name string, critical, fail bool) Step {
		return Step{Name: name, Critical: critical, Run: func(context.Context) (string, error) {
			ran[name] = true
			if fail {
				return "", errors.New("down")
			}
			return "ok", nil
		}}
	}
	res, err := RunChain(context.Background(),
		[]Step{mk("checkpoint", true, true), mk("compact", false, false), mk("rollup", false, false)},
		nil, quiet())
	if err == nil {
		t.Fatal("critical failure must fail the chain")
	}
	if !res.Aborted {
		t.Error("chain not marked aborted")
	}
	if ran["compact"] || ran["rollup"] {
		t.Errorf("downstream steps ran after critical failure: %v", ran)
	}
	if res.Steps[1].Status != "skipped" || res.Steps[2].Status != "skipped" {
		t.Errorf("downstream steps not recorded as skipped: %+v", res.Steps)
	}
}

func TestRunChainNilRecorderIsSafe(t *testing.T) {
	_, err := RunChain(context.Background(),
		[]Step{{Name: "x", Run: func(context.Context) (string, error) { return "", nil }}},
		nil, quiet())
	if err != nil {
		t.Fatalf("nil recorder: %v", err)
	}
}
