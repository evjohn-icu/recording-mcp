// Package worker claims processing jobs from the durable queue and runs them
// through an Executor while holding a lease. It is intentionally free of any
// domain logic: executors decide what a job means and return a result
// reference, while this package owns claiming, heartbeats and finalization.
package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"recordingmcp/internal/process"
)

const (
	defaultLease            = 30 * time.Second
	defaultPollInterval     = time.Second
	defaultProcessorVersion = "worker-v1"
	defaultStage            = "extract"
)

// Executor runs a single claimed job. It must honor ctx cancellation and
// return a result reference on success.
type Executor interface {
	Execute(ctx context.Context, job process.Job) (resultRef string, err error)
}

// Config configures Run and RunOnce. Zero values are replaced by defaults.
type Config struct {
	DB               *sql.DB
	WorkerID         string
	Stages           []string
	Lease            time.Duration
	ProcessorVersion string
	PollInterval     time.Duration
	Executor         Executor
}

func (c Config) withDefaults() Config {
	if c.Lease <= 0 {
		c.Lease = defaultLease
	}
	if c.PollInterval <= 0 {
		c.PollInterval = defaultPollInterval
	}
	if c.ProcessorVersion == "" {
		c.ProcessorVersion = defaultProcessorVersion
	}
	if len(c.Stages) == 0 {
		c.Stages = []string{defaultStage}
	}
	return c
}

func (c Config) validate() error {
	if c.DB == nil {
		return errors.New("worker: Config.DB is required")
	}
	if c.Executor == nil {
		return errors.New("worker: Config.Executor is required")
	}
	return nil
}

// RunOnce claims at most one job and processes it. It reports whether a job
// was claimed. An empty queue is not an error.
func RunOnce(ctx context.Context, cfg Config) (bool, error) {
	cfg = cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		return false, err
	}
	job, err := process.Claim(ctx, cfg.DB, process.ClaimOptions{
		WorkerID:         cfg.WorkerID,
		Stages:           cfg.Stages,
		Lease:            cfg.Lease,
		ProcessorVersion: cfg.ProcessorVersion,
		Now:              time.Now(),
	})
	if err != nil {
		if errors.Is(err, process.ErrNoJob) {
			return false, nil
		}
		return false, err
	}
	if job == nil {
		return false, nil
	}
	runJob(ctx, cfg, *job)
	return true, nil
}

// Run claims and processes jobs until ctx is cancelled, then returns nil.
// When the queue is empty it sleeps for PollInterval before polling again.
func Run(ctx context.Context, cfg Config) error {
	cfg = cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		return err
	}
	for {
		ran, err := RunOnce(ctx, cfg)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		if !ran {
			if !sleep(ctx, cfg.PollInterval) {
				return nil
			}
		}
	}
}

// runJob executes one claimed job with a heartbeat that renews the lease every
// lease/3. If the lease is lost the execution context is cancelled and the
// result is never submitted; the preempting worker owns the job from then on.
func runJob(parent context.Context, cfg Config, job process.Job) {
	execCtx, cancelExec := context.WithCancel(parent)
	defer cancelExec()

	var (
		mu        sync.Mutex
		leaseLost bool
		wg        sync.WaitGroup
	)
	done := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(heartbeatInterval(cfg.Lease))
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-parent.Done():
				return
			case <-ticker.C:
				if err := process.Renew(parent, cfg.DB, job.ID, job.LeaseToken, cfg.Lease, time.Now()); err != nil {
					if errors.Is(err, process.ErrLeaseLost) {
						mu.Lock()
						leaseLost = true
						mu.Unlock()
						cancelExec()
						return
					}
					// Transient renew errors are retried on the next tick.
				}
			}
		}
	}()

	resultRef, execErr := cfg.Executor.Execute(execCtx, job)
	close(done)
	wg.Wait()

	mu.Lock()
	lost := leaseLost
	mu.Unlock()
	if lost {
		return
	}

	// Finalize even when the parent context was cancelled by a shutdown so a
	// completed or failed attempt is recorded instead of waiting for lease
	// expiry.
	finalCtx := context.WithoutCancel(parent)
	now := time.Now()
	if execErr != nil {
		errorJSON, _ := json.Marshal(map[string]string{"error": execErr.Error()})
		_ = process.Fail(finalCtx, cfg.DB, job.ID, job.LeaseToken, string(errorJSON), true, now)
		return
	}
	_ = process.Complete(finalCtx, cfg.DB, job.ID, job.LeaseToken, resultRef, now)
}

// heartbeatInterval renews often enough to keep the lease alive but with a
// floor so degenerate leases do not spin.
func heartbeatInterval(lease time.Duration) time.Duration {
	if lease <= 0 {
		return time.Second
	}
	interval := lease / 3
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	return interval
}

// sleep waits for d or until ctx is cancelled, reporting whether the timer
// elapsed.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		d = time.Millisecond
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
