// Package notification owns the durable incident-email outbox and delivery worker.
package notification

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

const maximumAttempts = 4

var (
	ErrInvalidConfiguration = errors.New("invalid notification configuration")
	ErrLeaseLost            = errors.New("notification lease lost")
)

type DB interface {
	Begin(context.Context) (pgx.Tx, error)
}

type Message struct {
	DeliveryID    string
	IncidentID    string
	Recipient     string
	Transition    string
	Subject       string
	PlainTextBody string
}

type ProviderResponse struct {
	MessageID string
	Status    string
}

type Provider interface {
	Send(context.Context, Message) (ProviderResponse, error)
}

// ProviderFailure supplies a bounded status safe to persist. Error deliberately
// omits the underlying provider response, recipient, and credentials.
type ProviderFailure struct{ Status string }

func (failure ProviderFailure) Error() string { return "notification provider rejected delivery" }

type Config struct {
	WorkerID      string
	LeaseDuration time.Duration
}

type Option func(*Worker)

func WithClock(now func() time.Time) Option {
	return func(worker *Worker) {
		if now != nil {
			worker.now = now
		}
	}
}

type Worker struct {
	db       DB
	provider Provider
	workerID string
	lease    time.Duration
	now      func() time.Time
}

func NewWorker(db DB, provider Provider, config Config, options ...Option) (*Worker, error) {
	workerID := strings.TrimSpace(config.WorkerID)
	if db == nil || provider == nil || workerID == "" || len(workerID) > 128 ||
		config.LeaseDuration < time.Second || config.LeaseDuration > 5*time.Minute {
		return nil, ErrInvalidConfiguration
	}
	worker := &Worker{db: db, provider: provider, workerID: workerID, lease: config.LeaseDuration, now: time.Now}
	for _, option := range options {
		option(worker)
	}
	return worker, nil
}

// EnqueueIncidentEventTx snapshots verified, opted-in organization recipients
// in the same transaction as the incident transition.
func EnqueueIncidentEventTx(ctx context.Context, tx pgx.Tx, eventID, transition string) (int64, error) {
	if transition != "opened" && transition != "resolved" {
		return 0, ErrInvalidConfiguration
	}
	tag, err := tx.Exec(ctx, `INSERT INTO notification_outbox(
 organization_id,incident_id,incident_event_id,recipient_user_id,recipient_email,transition)
SELECT e.organization_id,e.incident_id,e.id,m.user_id,lower(btrim(u.email)),$2
FROM incident_events e
JOIN org_members m ON m.organization_id=e.organization_id
JOIN users u ON u.id=m.user_id
WHERE e.id=$1::uuid AND u.email_verified_at IS NOT NULL
  AND m.incident_notifications_enabled
  AND m.role IN('owner','admin','member','viewer')
ON CONFLICT(incident_event_id,recipient_user_id,channel) DO NOTHING`, eventID, transition)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

type claimedDelivery struct {
	deliveryID     string
	incidentID     string
	organizationID string
	environmentID  string
	recipient      string
	transition     string
	leaseToken     string
	attempt        int
}

// DeliverNext reclaims expired work, leases one due row with SKIP LOCKED,
// commits the claim, then calls the provider outside the database transaction.
func (worker *Worker) DeliverNext(ctx context.Context) (bool, error) {
	now := worker.now().UTC()
	if _, err := worker.ReclaimExpired(ctx, now); err != nil {
		return false, err
	}
	delivery, found, err := worker.claim(ctx, now)
	if err != nil || !found {
		return found, err
	}
	message := Message{
		DeliveryID: delivery.deliveryID,
		IncidentID: delivery.incidentID,
		Recipient:  delivery.recipient,
		Transition: delivery.transition,
		Subject:    "WatchTrace incident " + delivery.transition,
		PlainTextBody: fmt.Sprintf(
			"WatchTrace incident %s\nIncident ID: %s\nDelivery ID: %s\n",
			delivery.transition, delivery.incidentID, delivery.deliveryID),
	}
	response, providerErr := worker.provider.Send(ctx, message)
	attemptedAt := worker.now().UTC()
	if providerErr == nil {
		return true, worker.accept(ctx, delivery, attemptedAt, response)
	}
	return true, worker.retryOrFail(ctx, delivery, attemptedAt, safeProviderStatus(providerErr))
}

func (worker *Worker) ReclaimExpired(ctx context.Context, now time.Time) (int64, error) {
	tx, err := worker.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background())
	tag, err := tx.Exec(ctx, `UPDATE notification_outbox SET
 state='pending',next_attempt_at=LEAST(next_attempt_at,$1),lease_owner=NULL,lease_token=NULL,
 lease_expires_at=NULL,updated_at=$1
WHERE state='leased' AND lease_expires_at<=$1`, now.UTC())
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (worker *Worker) claim(ctx context.Context, now time.Time) (claimedDelivery, bool, error) {
	tx, err := worker.db.Begin(ctx)
	if err != nil {
		return claimedDelivery{}, false, err
	}
	defer tx.Rollback(context.Background())
	var delivery claimedDelivery
	err = tx.QueryRow(ctx, `WITH candidate AS (
 SELECT delivery_id FROM notification_outbox
 WHERE state='pending' AND next_attempt_at<=$1 AND attempt_count<4
 ORDER BY next_attempt_at,created_at,delivery_id
 FOR UPDATE SKIP LOCKED LIMIT 1
)
UPDATE notification_outbox o SET state='leased',lease_owner=$2,lease_token=gen_random_uuid(),
 lease_expires_at=$3,updated_at=$1
FROM candidate c WHERE o.delivery_id=c.delivery_id
RETURNING o.delivery_id::text,o.incident_id::text,o.organization_id::text,(SELECT environment_id::text FROM incidents WHERE id=o.incident_id),o.recipient_email,o.transition,
 o.lease_token::text,o.attempt_count+1`, now, worker.workerID, now.Add(worker.lease)).Scan(
		&delivery.deliveryID, &delivery.incidentID, &delivery.organizationID, &delivery.environmentID, &delivery.recipient, &delivery.transition,
		&delivery.leaseToken, &delivery.attempt)
	if errors.Is(err, pgx.ErrNoRows) {
		return claimedDelivery{}, false, nil
	}
	if err != nil {
		return claimedDelivery{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return claimedDelivery{}, false, err
	}
	return delivery, true, nil
}

func (worker *Worker) accept(ctx context.Context, delivery claimedDelivery, attemptedAt time.Time, response ProviderResponse) error {
	status := safeText(response.Status, "accepted", 160)
	messageID := safeText(response.MessageID, delivery.deliveryID, 255)
	tx, err := worker.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	tag, err := tx.Exec(ctx, `UPDATE notification_outbox SET state='accepted',attempt_count=$3,
 provider_message_id=$4,last_provider_status=$5,accepted_at=$6,
 lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL,updated_at=$6
WHERE delivery_id=$1::uuid AND state='leased' AND lease_token=$2::uuid`,
		delivery.deliveryID, delivery.leaseToken, delivery.attempt, messageID, status, attemptedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	if _, err = tx.Exec(ctx, `INSERT INTO notification_attempts(
 delivery_id,attempt_number,outcome,provider_status,attempted_at)
VALUES($1::uuid,$2,'accepted',$3,$4)`, delivery.deliveryID, delivery.attempt, status, attemptedAt); err != nil {
		return err
	}
	if err = insertRefreshEvent(ctx, tx, delivery); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (worker *Worker) retryOrFail(ctx context.Context, delivery claimedDelivery, attemptedAt time.Time, status string) error {
	final := delivery.attempt >= maximumAttempts
	state, outcome := "pending", "retry_scheduled"
	nextAttempt := attemptedAt
	if final {
		state, outcome = "failed", "failed"
	} else {
		nextAttempt = attemptedAt.Add(retryDelay(delivery.attempt))
	}
	tx, err := worker.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	tag, err := tx.Exec(ctx, `UPDATE notification_outbox SET state=$3,attempt_count=$4,
	 next_attempt_at=$5,last_provider_status=$6,failed_at=CASE WHEN $3::text='failed' THEN $7::timestamptz ELSE NULL END,
 lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL,updated_at=$7
WHERE delivery_id=$1::uuid AND state='leased' AND lease_token=$2::uuid`,
		delivery.deliveryID, delivery.leaseToken, state, delivery.attempt, nextAttempt, status, attemptedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	if _, err = tx.Exec(ctx, `INSERT INTO notification_attempts(
 delivery_id,attempt_number,outcome,provider_status,attempted_at)
VALUES($1::uuid,$2,$3,$4,$5)`, delivery.deliveryID, delivery.attempt, outcome, status, attemptedAt); err != nil {
		return err
	}
	if err = insertRefreshEvent(ctx, tx, delivery); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func insertRefreshEvent(ctx context.Context, tx pgx.Tx, delivery claimedDelivery) error {
	_, err := tx.Exec(ctx, `INSERT INTO api_refresh_events(organization_id,environment_id,event_type,resource_type,resource_id) VALUES($1::uuid,$2::uuid,'notification.changed','notification',$3::uuid)`, delivery.organizationID, delivery.environmentID, delivery.deliveryID)
	return err
}

func retryDelay(completedAttempt int) time.Duration {
	switch completedAttempt {
	case 1:
		return time.Minute
	case 2:
		return 5 * time.Minute
	case 3:
		return 25 * time.Minute
	default:
		return 0
	}
}

func safeProviderStatus(err error) string {
	var failure ProviderFailure
	if errors.As(err, &failure) {
		return safeText(failure.Status, "provider_error", 160)
	}
	return "provider_error"
}

func safeText(value, fallback string, maximum int) string {
	value = strings.TrimSpace(strings.Map(func(character rune) rune {
		if character < 32 || character == 127 {
			return -1
		}
		return character
	}, value))
	if value == "" {
		value = fallback
	}
	for len(value) > maximum {
		_, size := utf8.DecodeLastRuneInString(value)
		value = value[:len(value)-size]
	}
	return value
}
