// Package chaos provides a chaos testing framework for verifying
// system resilience under failure conditions.
//
// Chaos experiments:
//   - Kill a service pod
//   - Inject network latency
//   - Exhaust a resource (CPU, memory, connections)
//   - Corrupt data (invalid events)
//
// Usage:
//
//	runner := chaos.NewRunner(logger)
//	result := runner.Execute(ctx, chaos.KillPod("redis"))
package chaos

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os/exec"
	"time"
)

// Experiment defines a chaos test.
type Experiment struct {
	Name        string
	Description string
	Target      string // service or component
	Action      string // kill, latency, corrupt, exhaust
	Duration    time.Duration
	RunFn       func(ctx context.Context) error
	VerifyFn    func(ctx context.Context) error // verify system recovered
}

// Result is the outcome of a chaos experiment.
type Result struct {
	Experiment string
	Success    bool
	Duration   time.Duration
	Recovery   time.Duration
	Error      string
}

// Runner executes chaos experiments.
type Runner struct {
	log *slog.Logger
}

// NewRunner creates a chaos runner.
func NewRunner(log *slog.Logger) *Runner {
	return &Runner{log: log}
}

// Execute runs a chaos experiment and verifies recovery.
func (r *Runner) Execute(ctx context.Context, exp Experiment) Result {
	r.log.Info("chaos: starting experiment", "name", exp.Name, "target", exp.Target)
	start := time.Now()

	// Run the chaos action
	if err := exp.RunFn(ctx); err != nil {
		return Result{
			Experiment: exp.Name,
			Success:    false,
			Duration:   time.Since(start),
			Error:      "run failed: " + err.Error(),
		}
	}

	r.log.Info("chaos: action executed, waiting for recovery", "name", exp.Name)

	// Wait for duration if specified
	if exp.Duration > 0 {
		select {
		case <-time.After(exp.Duration):
		case <-ctx.Done():
		}
	}

	// Verify recovery
	recoveryStart := time.Now()
	if exp.VerifyFn != nil {
		if err := exp.VerifyFn(ctx); err != nil {
			return Result{
				Experiment: exp.Name,
				Success:    false,
				Duration:   time.Since(start),
				Recovery:   time.Since(recoveryStart),
				Error:      "verification failed: " + err.Error(),
			}
		}
	}

	r.log.Info("chaos: experiment complete", "name", exp.Name, "recovered", true)
	return Result{
		Experiment: exp.Name,
		Success:    true,
		Duration:   time.Since(start),
		Recovery:   time.Since(recoveryStart),
	}
}

// Pre-built experiments

// KillPod creates an experiment that deletes a K8s pod.
func KillPod(service string) Experiment {
	return Experiment{
		Name:        fmt.Sprintf("kill-%s", service),
		Description: fmt.Sprintf("Kill %s pod and verify recovery", service),
		Target:      service,
		Action:      "kill",
		Duration:    30 * time.Second,
		RunFn: func(ctx context.Context) error {
			cmd := exec.CommandContext(ctx, "kubectl", "-n", "adtech", "delete", "pod", "-l", "app="+service, "--force")
			return cmd.Run()
		},
		VerifyFn: func(ctx context.Context) error {
			// Check pod is back and healthy
			cmd := exec.CommandContext(ctx, "kubectl", "-n", "adtech", "wait", "--for=condition=ready", "pod", "-l", "app="+service, "--timeout=60s")
			return cmd.Run()
		},
	}
}

// InjectLatency creates an experiment that adds network delay.
func InjectLatency(service string, latency time.Duration, duration time.Duration) Experiment {
	return Experiment{
		Name:        fmt.Sprintf("latency-%s-%dms", service, latency.Milliseconds()),
		Description: fmt.Sprintf("Add %dms latency to %s", latency.Milliseconds(), service),
		Target:      service,
		Action:      "latency",
		Duration:    duration,
		RunFn: func(ctx context.Context) error {
			// In production: use tc (traffic control) or a service mesh
			// For local: just log the experiment
			return nil
		},
	}
}

// CorruptEvents creates an experiment that sends invalid events.
func CorruptEvents(trackerURL string, count int) Experiment {
	return Experiment{
		Name:        "corrupt-events",
		Description: fmt.Sprintf("Send %d malformed events to tracker", count),
		Target:      "tracker",
		Action:      "corrupt",
		RunFn: func(ctx context.Context) error {
			client := &http.Client{Timeout: 5 * time.Second}
			for i := 0; i < count; i++ {
				// Invalid trace ID, missing required fields
				url := fmt.Sprintf("%s/v1/t/imp?tid=&cid=&pid=", trackerURL)
				resp, err := client.Get(url)
				if err != nil {
					continue
				}
				resp.Body.Close()
			}
			return nil
		},
	}
}
