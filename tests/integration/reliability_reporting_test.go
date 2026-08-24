package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/watchtrace/watchtrace-platform/internal/reliability"
)

func TestReliabilityReportsScheduleHistoryBoundariesAndZeroDenominators(t *testing.T) {
	ctx, pool := openSchedulerTestPool(t)
	slugs := []string{"reliability-boundaries"}
	deleteSchedulerTestData(t, ctx, pool, slugs)
	t.Cleanup(func() { deleteSchedulerTestData(t, context.Background(), pool, slugs) })
	organizationID, environmentID := insertSchedulerTenant(t, ctx, pool, slugs[0])
	base := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Minute)
	monitorID := insertSchedulerMonitor(t, ctx, pool, organizationID, environmentID, "Reliability boundaries", 60, base.Add(time.Hour))

	insertSchedulePeriod(t, ctx, pool, organizationID, environmentID, monitorID, 1, 60, base, base, base.Add(5*time.Minute))
	// Five paused minutes create no expectations. The resumed monitor changes
	// interval and worker pool while preserving the earlier schedule as history.
	if _, err := pool.Exec(ctx, `INSERT INTO worker_pools(id,mode,enabled,lifecycle_state)
VALUES('reliability-customer','customer_vpc',true,'active') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM worker_pools WHERE id='reliability-customer'`)
	})
	insertSchedulePeriod(t, ctx, pool, organizationID, environmentID, monitorID, 2, 120, base.Add(10*time.Minute), base.Add(10*time.Minute), base.Add(16*time.Minute))
	if _, err := pool.Exec(ctx, `UPDATE monitor_schedule_periods SET worker_pool_id='reliability-customer'
WHERE monitor_id=$1::uuid AND monitor_version=2`, monitorID); err != nil {
		t.Fatal(err)
	}
	insertReliabilityResult(t, ctx, pool, organizationID, environmentID, monitorID, "scheduled", base, true)
	insertReliabilityResult(t, ctx, pool, organizationID, environmentID, monitorID, "scheduled", base.Add(time.Minute), false)
	insertReliabilityResult(t, ctx, pool, organizationID, environmentID, monitorID, "scheduled", base.Add(3*time.Minute), true)
	insertReliabilityResult(t, ctx, pool, organizationID, environmentID, monitorID, "scheduled", base.Add(4*time.Minute), false)
	insertReliabilityResult(t, ctx, pool, organizationID, environmentID, monitorID, "scheduled", base.Add(10*time.Minute), true)
	// A manual result on an expected slot must remain neither expected nor observed.
	insertReliabilityResult(t, ctx, pool, organizationID, environmentID, monitorID, "manual_test", base.Add(12*time.Minute), true)
	if _, err := pool.Exec(ctx, `INSERT INTO monitoring_coverage_gaps(organization_id,environment_id,monitor_id,scheduled_at,reason)
VALUES($1::uuid,$2::uuid,$3::uuid,$4,'missed')`, organizationID, environmentID, monitorID, base.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}

	service := reliability.New(pool)
	report, err := service.Report(ctx, monitorID, base, base.Add(20*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	assertReport(t, report, 8, 5, 3, 3, 0.6, 0.625)

	partial, err := service.Report(ctx, monitorID, base.Add(30*time.Second), base.Add(12*time.Minute+30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	assertReport(t, partial, 6, 4, 2, 2, 0.5, 4.0/6.0)

	paused, err := service.Report(ctx, monitorID, base.Add(5*time.Minute), base.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if paused.Expected != 0 || paused.Observed != 0 || paused.Unknown != 0 || paused.Coverage != nil || paused.ObservedUptime != nil {
		t.Fatalf("paused no-data report=%+v", paused)
	}

	missing, err := service.Report(ctx, monitorID, base.Add(14*time.Minute), base.Add(16*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if missing.Expected != 1 || missing.Observed != 0 || missing.Unknown != 1 || missing.ObservedUptime != nil || missing.Coverage == nil || *missing.Coverage != 0 {
		t.Fatalf("missing report=%+v", missing)
	}
}

func TestOrderedStateRecomputesLateResultsAndUnknownPausesCounters(t *testing.T) {
	ctx, pool := openSchedulerTestPool(t)
	slugs := []string{"ordered-state-correction"}
	deleteSchedulerTestData(t, ctx, pool, slugs)
	t.Cleanup(func() { deleteSchedulerTestData(t, context.Background(), pool, slugs) })
	organizationID, environmentID := insertSchedulerTenant(t, ctx, pool, slugs[0])
	base := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Minute)
	monitorID := insertSchedulerMonitor(t, ctx, pool, organizationID, environmentID, "Ordered state", 60, base.Add(time.Hour))
	insertSchedulePeriod(t, ctx, pool, organizationID, environmentID, monitorID, 1, 60, base, base, base.Add(7*time.Minute))
	service := reliability.New(pool)

	job0 := insertReliabilityResult(t, ctx, pool, organizationID, environmentID, monitorID, "scheduled", base, false)
	evaluateAccepted(t, ctx, service, monitorID, job0, base, base.Add(30*time.Second))
	job1 := insertReliabilityResult(t, ctx, pool, organizationID, environmentID, monitorID, "scheduled", base.Add(time.Minute), false)
	evaluateAccepted(t, ctx, service, monitorID, job1, base.Add(time.Minute), base.Add(time.Minute+30*time.Second))
	job3 := insertReliabilityResult(t, ctx, pool, organizationID, environmentID, monitorID, "scheduled", base.Add(3*time.Minute), false)
	evaluateAccepted(t, ctx, service, monitorID, job3, base.Add(3*time.Minute), base.Add(3*time.Minute+30*time.Second))
	assertReliabilityState(t, ctx, pool, monitorID, "down", "down", 3, 0)

	// Filling the unknown slot with a success changes the ordered sequence from
	// fail/fail/unknown/fail to fail/fail/success/fail.
	job2 := insertReliabilityResult(t, ctx, pool, organizationID, environmentID, monitorID, "scheduled", base.Add(2*time.Minute), true)
	corrected, err := service.EvaluateAccepted(ctx, monitorID, job2, base.Add(2*time.Minute), base.Add(4*time.Minute), base.Add(3*time.Minute+45*time.Second))
	if err != nil || !corrected {
		t.Fatalf("late correction corrected=%t error=%v", corrected, err)
	}
	assertReliabilityState(t, ctx, pool, monitorID, "degraded", "degraded", 1, 0)

	// Replaying the same accepted correction is idempotent and does not create
	// another audit event.
	corrected, err = service.EvaluateAccepted(ctx, monitorID, job2, base.Add(2*time.Minute), base.Add(4*time.Minute), base.Add(3*time.Minute+50*time.Second))
	if err != nil || !corrected {
		t.Fatalf("replayed correction corrected=%t error=%v", corrected, err)
	}
	var correctionEvents int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM monitor_state_correction_events WHERE monitor_id=$1::uuid`, monitorID).Scan(&correctionEvents); err != nil || correctionEvents != 1 {
		t.Fatalf("correction events=%d error=%v", correctionEvents, err)
	}

	// The next due slot is unknown. It changes only the displayed state and
	// pauses the underlying consecutive-failure counter at one.
	if err = service.EvaluateMonitor(ctx, monitorID, base.Add(4*time.Minute+30*time.Second)); err != nil {
		t.Fatal(err)
	}
	assertReliabilityState(t, ctx, pool, monitorID, "unknown", "degraded", 1, 0)

	// A result outside the deadline plus ten-minute correction window remains
	// raw data and cannot rewrite the newer monitor state.
	job4 := insertReliabilityResult(t, ctx, pool, organizationID, environmentID, monitorID, "scheduled", base.Add(4*time.Minute), true)
	corrected, err = service.EvaluateAccepted(ctx, monitorID, job4, base.Add(4*time.Minute), base.Add(6*time.Minute), base.Add(17*time.Minute))
	if err != nil || corrected {
		t.Fatalf("too-late result corrected=%t error=%v", corrected, err)
	}
	assertReliabilityState(t, ctx, pool, monitorID, "unknown", "degraded", 1, 0)
}

func TestLateResultInvalidationRepairsHourlyAndDailyRollupsRepeatably(t *testing.T) {
	ctx, pool := openSchedulerTestPool(t)
	slugs := []string{"rollup-repair"}
	deleteSchedulerTestData(t, ctx, pool, slugs)
	t.Cleanup(func() { deleteSchedulerTestData(t, context.Background(), pool, slugs) })
	organizationID, environmentID := insertSchedulerTenant(t, ctx, pool, slugs[0])
	base := time.Now().UTC().Add(-26 * time.Hour).Truncate(time.Hour)
	monitorID := insertSchedulerMonitor(t, ctx, pool, organizationID, environmentID, "Rollup repair", 60, time.Now().UTC().Add(time.Hour))
	insertSchedulePeriod(t, ctx, pool, organizationID, environmentID, monitorID, 1, 60, base, base, base.Add(3*time.Minute))
	insertReliabilityResult(t, ctx, pool, organizationID, environmentID, monitorID, "scheduled", base, true)
	service := reliability.New(pool)
	if _, err := service.RollupHour(ctx, base); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RollupDay(ctx, base); err != nil {
		t.Fatal(err)
	}

	insertReliabilityResult(t, ctx, pool, organizationID, environmentID, monitorID, "scheduled", base.Add(time.Minute), false)
	if _, err := pool.Exec(ctx, `INSERT INTO monitor_rollup_invalidations(monitor_id,bucket_kind,bucket_start,reason) VALUES
($1::uuid,'hourly',$2,'late_result'),($1::uuid,'daily',date_trunc('day',$2::timestamptz AT TIME ZONE 'UTC') AT TIME ZONE 'UTC','late_result')`, monitorID, base); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RollupDay(ctx, base); err == nil {
		t.Fatal("daily rollup advanced before hourly repair")
	}
	if repaired, err := service.RepairInvalidated(ctx, 10); err != nil || repaired != 2 {
		t.Fatalf("repaired=%d error=%v", repaired, err)
	}
	if repaired, err := service.RepairInvalidated(ctx, 10); err != nil || repaired != 0 {
		t.Fatalf("repeat repaired=%d error=%v", repaired, err)
	}
	var hourlyExpected, hourlyObserved, hourlySuccess, hourlyUnknown int
	if err := pool.QueryRow(ctx, `SELECT expected_checks,observed_checks,successful_checks,unknown_checks
FROM monitor_rollups_hourly WHERE monitor_id=$1::uuid AND bucket_start=$2`, monitorID, base).Scan(
		&hourlyExpected, &hourlyObserved, &hourlySuccess, &hourlyUnknown); err != nil {
		t.Fatal(err)
	}
	if hourlyExpected != 3 || hourlyObserved != 2 || hourlySuccess != 1 || hourlyUnknown != 1 {
		t.Fatalf("hourly rollup expected=%d observed=%d success=%d unknown=%d", hourlyExpected, hourlyObserved, hourlySuccess, hourlyUnknown)
	}
	var dailyObserved int
	if err := pool.QueryRow(ctx, `SELECT observed_checks FROM monitor_rollups_daily WHERE monitor_id=$1::uuid AND bucket_start=$2::date`, monitorID, base).Scan(&dailyObserved); err != nil || dailyObserved != 2 {
		t.Fatalf("daily observed=%d error=%v", dailyObserved, err)
	}
}

func TestRollupCheckpointCatchUpIsBoundedAndRestartSafe(t *testing.T) {
	ctx, pool := openSchedulerTestPool(t)
	var originalHour time.Time
	var originalDay time.Time
	if err := pool.QueryRow(ctx, `SELECT hourly_through,daily_through::timestamptz FROM monitoring_rollup_checkpoint WHERE singleton`).Scan(&originalHour, &originalDay); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `UPDATE monitoring_rollup_checkpoint SET hourly_through=$1,daily_through=$2::date WHERE singleton`, originalHour, originalDay)
	})
	now := time.Now().UTC().Truncate(time.Hour)
	start := now.Add(-3 * time.Hour)
	if _, err := pool.Exec(ctx, `UPDATE monitoring_rollup_checkpoint SET hourly_through=$1,daily_through=$2::date WHERE singleton`, start, now); err != nil {
		t.Fatal(err)
	}
	service := reliability.New(pool)
	if err := service.AdvanceRollups(ctx, now, 1, 0); err != nil {
		t.Fatal(err)
	}
	var first time.Time
	if err := pool.QueryRow(ctx, `SELECT hourly_through FROM monitoring_rollup_checkpoint WHERE singleton`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if !first.Equal(start.Add(time.Hour)) {
		t.Fatalf("first bounded catch-up=%s", first)
	}
	// A restarted process resumes from the durable checkpoint. Re-running an
	// already materialized bucket is harmless because rollup writes are upserts.
	if _, err := service.RollupHour(ctx, first.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := service.AdvanceRollups(ctx, now, 1, 0); err != nil {
		t.Fatal(err)
	}
	var second time.Time
	if err := pool.QueryRow(ctx, `SELECT hourly_through FROM monitoring_rollup_checkpoint WHERE singleton`).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if !second.Equal(first.Add(time.Hour)) {
		t.Fatalf("restart catch-up=%s", second)
	}
}

func insertSchedulePeriod(t *testing.T, ctx context.Context, pool *pgxpool.Pool, organizationID, environmentID, monitorID string, version, interval int, starts, first, ends time.Time) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO monitor_schedule_periods(
organization_id,environment_id,monitor_id,monitor_version,interval_seconds,worker_pool_id,starts_at,first_slot_at,ends_at)
VALUES($1::uuid,$2::uuid,$3::uuid,$4,$5,'hosted',$6,$7,$8)`, organizationID, environmentID, monitorID, version, interval, starts, first, ends); err != nil {
		t.Fatal(err)
	}
}

func insertReliabilityResult(t *testing.T, ctx context.Context, pool *pgxpool.Pool, organizationID, environmentID, monitorID, jobType string, scheduled time.Time, succeeded bool) string {
	t.Helper()
	var jobID string
	var status any
	var category any
	if succeeded {
		status = int16(204)
	} else {
		category = "timeout"
	}
	if err := pool.QueryRow(ctx, `INSERT INTO check_jobs(
organization_id,environment_id,monitor_id,job_type,state,scheduled_at,started_at,completed_at,expires_at)
VALUES($1::uuid,$2::uuid,$3::uuid,$4,'completed',$5::timestamptz,$5::timestamptz,$5::timestamptz,$5::timestamptz+INTERVAL '2 minutes') RETURNING id::text`,
		organizationID, environmentID, monitorID, jobType, scheduled).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO health_checks(
job_id,organization_id,environment_id,monitor_id,job_type,scheduled_at,started_at,completed_at,
succeeded,status_code,error_category,total_duration_microseconds)
VALUES($1::uuid,$2::uuid,$3::uuid,$4::uuid,$5,$6,$6,$6,$7,$8,$9,1000)`,
		jobID, organizationID, environmentID, monitorID, jobType, scheduled, succeeded, status, category); err != nil {
		t.Fatal(err)
	}
	return jobID
}

func evaluateAccepted(t *testing.T, ctx context.Context, service *reliability.Service, monitorID, jobID string, scheduled, now time.Time) {
	t.Helper()
	if _, err := service.EvaluateAccepted(ctx, monitorID, jobID, scheduled, scheduled.Add(2*time.Minute), now); err != nil {
		t.Fatal(err)
	}
}

func assertReport(t *testing.T, report reliability.Report, expected, observed, successful, unknown int64, uptime, coverage float64) {
	t.Helper()
	if report.Expected != expected || report.Observed != observed || report.Successful != successful || report.Unknown != unknown || report.ObservedUptime == nil || report.Coverage == nil || *report.ObservedUptime != uptime || *report.Coverage != coverage {
		t.Fatalf("report=%+v", report)
	}
}

func assertReliabilityState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, monitorID, display, observed string, failures, successes int) {
	t.Helper()
	var gotDisplay, gotObserved string
	var gotFailures, gotSuccesses int
	if err := pool.QueryRow(ctx, `SELECT display_state,observed_state,consecutive_failures,consecutive_successes
FROM monitor_reliability_states WHERE monitor_id=$1::uuid`, monitorID).Scan(&gotDisplay, &gotObserved, &gotFailures, &gotSuccesses); err != nil {
		t.Fatal(err)
	}
	if gotDisplay != display || gotObserved != observed || gotFailures != failures || gotSuccesses != successes {
		t.Fatalf("state display=%s observed=%s failures=%d successes=%d", gotDisplay, gotObserved, gotFailures, gotSuccesses)
	}
}
