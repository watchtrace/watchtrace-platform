// Package backendapi implements the bounded customer-facing Phase 1 read model.
package backendapi

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/watchtrace/watchtrace-platform/internal/authorization"
	"github.com/watchtrace/watchtrace-platform/internal/incident"
	"github.com/watchtrace/watchtrace-platform/internal/reliability"
)

var (
	ErrNotFound     = errors.New("resource not found")
	ErrForbidden    = errors.New("permission denied")
	ErrInvalidQuery = errors.New("invalid bounded query")
)

type DB interface {
	Begin(context.Context) (pgx.Tx, error)
}
type Service struct {
	db          DB
	incidents   *incident.Service
	reliability *reliability.Service
}

func New(db DB) *Service {
	return &Service{db: db, incidents: incident.NewService(db), reliability: reliability.New(db)}
}

type PageQuery struct {
	Limit                   int
	From, To                time.Time
	Cursor, Status, JobType string
}
type Check struct {
	JobID                     string    `json:"job_id"`
	JobType                   string    `json:"job_type"`
	ScheduledAt               time.Time `json:"scheduled_at"`
	StartedAt                 time.Time `json:"started_at"`
	CompletedAt               time.Time `json:"completed_at"`
	Succeeded                 bool      `json:"succeeded"`
	StatusCode                *int16    `json:"status_code"`
	ErrorCategory             *string   `json:"error_category"`
	TotalDurationMicroseconds int64     `json:"total_duration_us"`
}
type CheckPage struct {
	Items      []Check `json:"items"`
	NextCursor *string `json:"next_cursor"`
}
type Report struct {
	From                       time.Time  `json:"from"`
	To                         time.Time  `json:"to"`
	Expected                   int64      `json:"expected"`
	Observed                   int64      `json:"observed"`
	Successful                 int64      `json:"successful"`
	Unknown                    int64      `json:"unknown"`
	ObservedUptime             *float64   `json:"observed_uptime"`
	Coverage                   *float64   `json:"coverage"`
	AverageLatencyMilliseconds *float64   `json:"average_latency_ms"`
	Fresh                      bool       `json:"fresh"`
	CorrectedAt                *time.Time `json:"corrected_at"`
}
type StateCounts struct {
	Healthy  int64 `json:"healthy"`
	Degraded int64 `json:"degraded"`
	Down     int64 `json:"down"`
	Unknown  int64 `json:"unknown"`
}
type Dashboard struct {
	States        StateCounts `json:"states"`
	Reliability   Report      `json:"reliability"`
	OpenIncidents int64       `json:"open_incidents"`
	GeneratedAt   time.Time   `json:"generated_at"`
}
type IncidentEvent struct {
	ID          string    `json:"id"`
	Type        string    `json:"type"`
	ActorUserID *string   `json:"actor_user_id"`
	SourceJobID *string   `json:"source_job_id"`
	Reason      *string   `json:"reason"`
	OccurredAt  time.Time `json:"occurred_at"`
}
type Delivery struct {
	ID             string     `json:"id"`
	Transition     string     `json:"transition"`
	State          string     `json:"state"`
	Attempts       int16      `json:"attempts"`
	NextAttemptAt  time.Time  `json:"next_attempt_at"`
	ProviderStatus *string    `json:"provider_status"`
	AcceptedAt     *time.Time `json:"accepted_at"`
	FailedAt       *time.Time `json:"failed_at"`
}
type IncidentSummary struct {
	Incident   incident.Incident `json:"incident"`
	Events     []IncidentEvent   `json:"events"`
	Deliveries []Delivery        `json:"deliveries"`
}
type IncidentPage struct {
	Items      []incident.Incident `json:"items"`
	NextCursor *string             `json:"next_cursor"`
}

func normalizeQuery(q PageQuery, maxWindow time.Duration) (PageQuery, error) {
	if q.Limit == 0 {
		q.Limit = 50
	}
	if q.Limit < 1 || q.Limit > 100 {
		return q, ErrInvalidQuery
	}
	q.From, q.To = q.From.UTC(), q.To.UTC()
	if q.From.IsZero() {
		q.From = time.Now().UTC().Add(-24 * time.Hour)
	}
	if q.To.IsZero() {
		q.To = time.Now().UTC()
	}
	if !q.To.After(q.From) || q.To.Sub(q.From) > maxWindow {
		return q, ErrInvalidQuery
	}
	return q, nil
}
func (s *Service) authorizeEnvironment(ctx context.Context, tx pgx.Tx, userID, environmentID string, permission authorization.Permission) (string, authorization.Role, error) {
	var org string
	var role authorization.Role
	err := tx.QueryRow(ctx, `SELECT e.organization_id::text,m.role FROM environments e JOIN organizations o ON o.id=e.organization_id AND o.deleted_at IS NULL JOIN org_members m ON m.organization_id=e.organization_id AND m.user_id=$1::uuid WHERE e.id=$2::uuid`, userID, environmentID).Scan(&org, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", err
	}
	if !authorization.Allows(role, permission) {
		return "", "", ErrForbidden
	}
	return org, role, nil
}
func authorizeMonitor(ctx context.Context, tx pgx.Tx, userID, environmentID, monitorID string) (string, error) {
	var org string
	var role authorization.Role
	err := tx.QueryRow(ctx, `SELECT m.organization_id::text,om.role FROM monitors m JOIN organizations o ON o.id=m.organization_id AND o.deleted_at IS NULL JOIN org_members om ON om.organization_id=m.organization_id AND om.user_id=$1::uuid WHERE m.environment_id=$2::uuid AND m.id=$3::uuid AND m.deleted_at IS NULL`, userID, environmentID, monitorID).Scan(&org, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if !authorization.Allows(role, authorization.PermissionMonitorsRead) {
		return "", ErrForbidden
	}
	return org, nil
}

func (s *Service) ListChecks(ctx context.Context, userID, environmentID, monitorID string, q PageQuery) (CheckPage, error) {
	q, err := normalizeQuery(q, 31*24*time.Hour)
	if err != nil {
		return CheckPage{}, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return CheckPage{}, err
	}
	defer tx.Rollback(context.Background())
	if _, err = authorizeMonitor(ctx, tx, userID, environmentID, monitorID); err != nil {
		return CheckPage{}, err
	}
	var cursorTime time.Time
	var cursorID string
	if q.Cursor != "" {
		cursorTime, cursorID, err = decodeCursor(q.Cursor)
		if err != nil {
			return CheckPage{}, ErrInvalidQuery
		}
	}
	rows, err := tx.Query(ctx, `SELECT job_id::text,job_type,scheduled_at,started_at,completed_at,succeeded,status_code,error_category,total_duration_microseconds FROM health_checks WHERE monitor_id=$1::uuid AND scheduled_at>=$2 AND scheduled_at<$3 AND ($4='' OR job_type=$4) AND ($5::timestamptz IS NULL OR (scheduled_at,job_id)<($5,$6::uuid)) ORDER BY scheduled_at DESC,job_id DESC LIMIT $7`, monitorID, q.From, q.To, q.JobType, nullableTime(cursorTime), nullableString(cursorID), q.Limit+1)
	if err != nil {
		return CheckPage{}, err
	}
	defer rows.Close()
	items := []Check{}
	for rows.Next() {
		var v Check
		if err = rows.Scan(&v.JobID, &v.JobType, &v.ScheduledAt, &v.StartedAt, &v.CompletedAt, &v.Succeeded, &v.StatusCode, &v.ErrorCategory, &v.TotalDurationMicroseconds); err != nil {
			return CheckPage{}, err
		}
		if v.JobType == "manual_test" {
			v.JobType = "manual"
		}
		items = append(items, v)
	}
	if err = rows.Err(); err != nil {
		return CheckPage{}, err
	}
	page := CheckPage{Items: items}
	if len(items) > q.Limit {
		last := items[q.Limit-1]
		cursor := encodeCursor(last.ScheduledAt, last.JobID)
		page.NextCursor = &cursor
		page.Items = items[:q.Limit]
	}
	return page, nil
}

func (s *Service) MonitorReport(ctx context.Context, userID, environmentID, monitorID string, q PageQuery) (Report, error) {
	q, err := normalizeQuery(q, 366*24*time.Hour)
	if err != nil {
		return Report{}, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Report{}, err
	}
	if _, err = authorizeMonitor(ctx, tx, userID, environmentID, monitorID); err != nil {
		tx.Rollback(context.Background())
		return Report{}, err
	}
	tx.Rollback(context.Background())
	base, err := s.reliability.Report(ctx, monitorID, q.From, q.To)
	if err != nil {
		return Report{}, err
	}
	report := Report{From: q.From, To: q.To, Expected: base.Expected, Observed: base.Observed, Successful: base.Successful, Unknown: base.Unknown, ObservedUptime: base.ObservedUptime, Coverage: base.Coverage, Fresh: true}
	tx, err = s.db.Begin(ctx)
	if err != nil {
		return report, err
	}
	defer tx.Rollback(context.Background())
	var average *float64
	if err = tx.QueryRow(ctx, `SELECT avg(total_duration_microseconds)::float8/1000 FROM health_checks WHERE monitor_id=$1::uuid AND job_type='scheduled' AND scheduled_at>=$2 AND scheduled_at<$3`, monitorID, q.From, q.To).Scan(&average); err != nil {
		return report, err
	}
	report.AverageLatencyMilliseconds = average
	var invalidated *time.Time
	if err = tx.QueryRow(ctx, `SELECT min(invalidated_at) FROM monitor_rollup_invalidations WHERE monitor_id=$1::uuid AND bucket_start<$3 AND bucket_start+CASE bucket_kind WHEN 'hourly' THEN INTERVAL '1 hour' ELSE INTERVAL '1 day' END>$2`, monitorID, q.From, q.To).Scan(&invalidated); err != nil {
		return report, err
	}
	if invalidated != nil {
		report.Fresh = false
		report.CorrectedAt = invalidated
	}
	return report, nil
}

func (s *Service) Dashboard(ctx context.Context, userID, environmentID string, q PageQuery) (Dashboard, error) {
	q, err := normalizeQuery(q, 31*24*time.Hour)
	if err != nil {
		return Dashboard{}, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Dashboard{}, err
	}
	defer tx.Rollback(context.Background())
	if _, _, err = s.authorizeEnvironment(ctx, tx, userID, environmentID, authorization.PermissionMonitorsRead); err != nil {
		return Dashboard{}, err
	}
	var d Dashboard
	d.GeneratedAt = time.Now().UTC()
	err = tx.QueryRow(ctx, `SELECT count(*)FILTER(WHERE COALESCE(r.display_state,'unknown')='healthy'),count(*)FILTER(WHERE COALESCE(r.display_state,'unknown')='degraded'),count(*)FILTER(WHERE COALESCE(r.display_state,'unknown')='down'),count(*)FILTER(WHERE COALESCE(r.display_state,'unknown')='unknown') FROM monitors m LEFT JOIN monitor_reliability_states r ON r.monitor_id=m.id WHERE m.environment_id=$1::uuid AND m.deleted_at IS NULL`, environmentID).Scan(&d.States.Healthy, &d.States.Degraded, &d.States.Down, &d.States.Unknown)
	if err != nil {
		return d, err
	}
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM incidents WHERE environment_id=$1::uuid AND status='open'`, environmentID).Scan(&d.OpenIncidents); err != nil {
		return d, err
	}
	err = tx.QueryRow(ctx, `WITH expected AS (SELECT p.monitor_id,slot FROM monitor_schedule_periods p CROSS JOIN LATERAL generate_series(p.first_slot_at+GREATEST(0,ceil(EXTRACT(EPOCH FROM ($2-p.first_slot_at))/p.interval_seconds)::bigint)*make_interval(secs=>p.interval_seconds),LEAST(COALESCE(p.ends_at,$3),$3)-INTERVAL '1 microsecond',make_interval(secs=>p.interval_seconds)) slot WHERE p.environment_id=$1::uuid AND p.starts_at<$3 AND COALESCE(p.ends_at,$3)>$2 AND slot>=GREATEST($2,p.starts_at)),a AS(SELECT count(*)::bigint expected,count(h.job_id)::bigint observed,count(h.job_id)FILTER(WHERE h.succeeded)::bigint successful,avg(h.total_duration_microseconds)::float8/1000 latency FROM expected e LEFT JOIN health_checks h ON h.monitor_id=e.monitor_id AND h.job_type='scheduled' AND h.scheduled_at=e.slot) SELECT expected,observed,successful,latency FROM a`, environmentID, q.From, q.To).Scan(&d.Reliability.Expected, &d.Reliability.Observed, &d.Reliability.Successful, &d.Reliability.AverageLatencyMilliseconds)
	if err != nil {
		return d, err
	}
	normalized := reliability.Report{Expected: d.Reliability.Expected, Observed: d.Reliability.Observed, Successful: d.Reliability.Successful}.Normalize()
	d.Reliability.From = q.From
	d.Reliability.To = q.To
	d.Reliability.Unknown = normalized.Unknown
	d.Reliability.ObservedUptime = normalized.ObservedUptime
	d.Reliability.Coverage = normalized.Coverage
	d.Reliability.Fresh = true
	var invalidated *time.Time
	if err = tx.QueryRow(ctx, `SELECT min(i.invalidated_at) FROM monitor_rollup_invalidations i JOIN monitors m ON m.id=i.monitor_id WHERE m.environment_id=$1::uuid AND i.bucket_start<$3 AND i.bucket_start+CASE i.bucket_kind WHEN 'hourly' THEN INTERVAL '1 hour' ELSE INTERVAL '1 day' END>$2`, environmentID, q.From, q.To).Scan(&invalidated); err != nil {
		return d, err
	}
	if invalidated != nil {
		d.Reliability.Fresh = false
		d.Reliability.CorrectedAt = invalidated
	}
	return d, nil
}

func (s *Service) ListIncidents(ctx context.Context, userID, environmentID string, q PageQuery) (IncidentPage, error) {
	q, err := normalizeQuery(q, 366*24*time.Hour)
	if err != nil {
		return IncidentPage{}, err
	}
	if q.Status != "" && q.Status != "open" && q.Status != "resolved" {
		return IncidentPage{}, ErrInvalidQuery
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return IncidentPage{}, err
	}
	defer tx.Rollback(context.Background())
	if _, _, err = s.authorizeEnvironment(ctx, tx, userID, environmentID, authorization.PermissionIncidentsRead); err != nil {
		return IncidentPage{}, err
	}
	var cursorTime time.Time
	var cursorID string
	if q.Cursor != "" {
		cursorTime, cursorID, err = decodeCursor(q.Cursor)
		if err != nil {
			return IncidentPage{}, ErrInvalidQuery
		}
	}
	rows, err := tx.Query(ctx, `SELECT id::text,organization_id::text,environment_id::text,monitor_id::text,status,started_at,opened_at,acknowledged_at,acknowledged_by_user_id::text,resolved_at,resolved_by_user_id::text,resolution_kind,resolution_reason FROM incidents WHERE environment_id=$1::uuid AND opened_at>=$2 AND opened_at<$3 AND ($4='' OR status=$4) AND ($5::timestamptz IS NULL OR (opened_at,id)<($5,$6::uuid)) ORDER BY opened_at DESC,id DESC LIMIT $7`, environmentID, q.From, q.To, q.Status, nullableTime(cursorTime), nullableString(cursorID), q.Limit+1)
	if err != nil {
		return IncidentPage{}, err
	}
	defer rows.Close()
	items := []incident.Incident{}
	for rows.Next() {
		var v incident.Incident
		if err = rows.Scan(&v.ID, &v.OrganizationID, &v.EnvironmentID, &v.MonitorID, &v.Status, &v.StartedAt, &v.OpenedAt, &v.AcknowledgedAt, &v.AcknowledgedByUserID, &v.ResolvedAt, &v.ResolvedByUserID, &v.ResolutionKind, &v.ResolutionReason); err != nil {
			return IncidentPage{}, err
		}
		items = append(items, v)
	}
	page := IncidentPage{Items: items}
	if len(items) > q.Limit {
		last := items[q.Limit-1]
		c := encodeCursor(last.OpenedAt, last.ID)
		page.NextCursor = &c
		page.Items = items[:q.Limit]
	}
	return page, rows.Err()
}

func (s *Service) GetIncident(ctx context.Context, userID, environmentID, incidentID string) (IncidentSummary, error) {
	base, err := s.incidents.Get(ctx, userID, environmentID, incidentID)
	if err != nil {
		return IncidentSummary{}, mapIncidentError(err)
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return IncidentSummary{}, err
	}
	defer tx.Rollback(context.Background())
	result := IncidentSummary{Incident: base, Events: []IncidentEvent{}, Deliveries: []Delivery{}}
	rows, err := tx.Query(ctx, `SELECT id::text,event_type,actor_user_id::text,source_job_id::text,safe_reason,occurred_at FROM incident_events WHERE incident_id=$1::uuid ORDER BY occurred_at,id LIMIT 200`, incidentID)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var v IncidentEvent
		if err = rows.Scan(&v.ID, &v.Type, &v.ActorUserID, &v.SourceJobID, &v.Reason, &v.OccurredAt); err != nil {
			rows.Close()
			return result, err
		}
		result.Events = append(result.Events, v)
	}
	rows.Close()
	rows, err = tx.Query(ctx, `SELECT delivery_id::text,transition,state,attempt_count,next_attempt_at,last_provider_status,accepted_at,failed_at FROM notification_outbox WHERE incident_id=$1::uuid ORDER BY created_at,delivery_id LIMIT 200`, incidentID)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var v Delivery
		if err = rows.Scan(&v.ID, &v.Transition, &v.State, &v.Attempts, &v.NextAttemptAt, &v.ProviderStatus, &v.AcceptedAt, &v.FailedAt); err != nil {
			return result, err
		}
		result.Deliveries = append(result.Deliveries, v)
	}
	return result, rows.Err()
}
func (s *Service) Acknowledge(ctx context.Context, userID, environmentID, incidentID, reason string) (IncidentSummary, error) {
	if _, err := s.incidents.Acknowledge(ctx, userID, environmentID, incidentID, reason); err != nil {
		return IncidentSummary{}, mapIncidentError(err)
	}
	return s.GetIncident(ctx, userID, environmentID, incidentID)
}
func (s *Service) Resolve(ctx context.Context, userID, environmentID, incidentID, reason string) (IncidentSummary, error) {
	if _, err := s.incidents.Resolve(ctx, userID, environmentID, incidentID, reason); err != nil {
		return IncidentSummary{}, mapIncidentError(err)
	}
	return s.GetIncident(ctx, userID, environmentID, incidentID)
}
func mapIncidentError(err error) error {
	if errors.Is(err, incident.ErrIncidentNotFound) {
		return ErrNotFound
	}
	if errors.Is(err, incident.ErrForbidden) {
		return ErrForbidden
	}
	if errors.Is(err, incident.ErrInvalidInput) || errors.Is(err, incident.ErrAlreadyResolved) {
		return ErrInvalidQuery
	}
	return err
}
func encodeCursor(at time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(at.UTC().Format(time.RFC3339Nano) + "|" + id))
}
func decodeCursor(value string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return time.Time{}, "", err
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 2 || len(parts[1]) != 36 {
		return time.Time{}, "", ErrInvalidQuery
	}
	if _, err = uuid.Parse(parts[1]); err != nil {
		return time.Time{}, "", ErrInvalidQuery
	}
	at, err := time.Parse(time.RFC3339Nano, parts[0])
	return at, parts[1], err
}
func nullableTime(v time.Time) any {
	if v.IsZero() {
		return nil
	}
	return v
}
func nullableString(v string) any {
	if v == "" {
		return nil
	}
	return v
}
