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
	"github.com/jackc/pgx/v5/pgtype"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
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
	return database.New(tx).EnqueueIncidentNotifications(ctx, database.EnqueueIncidentNotificationsParams{Transition: transition, EventID: eventID})
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
	rowsAffected, err := database.New(tx).ReclaimExpiredNotificationLeases(ctx, notificationTimestamp(now.UTC()))
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return rowsAffected, nil
}

func (worker *Worker) claim(ctx context.Context, now time.Time) (claimedDelivery, bool, error) {
	tx, err := worker.db.Begin(ctx)
	if err != nil {
		return claimedDelivery{}, false, err
	}
	defer tx.Rollback(context.Background())
	row, err := database.New(tx).ClaimNotificationDelivery(ctx, database.ClaimNotificationDeliveryParams{
		WorkerID:       pgtype.Text{String: worker.workerID, Valid: true},
		LeaseExpiresAt: notificationTimestamp(now.Add(worker.lease)), ClaimedAt: notificationTimestamp(now),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return claimedDelivery{}, false, nil
	}
	if err != nil {
		return claimedDelivery{}, false, err
	}
	delivery := claimedDelivery{deliveryID: row.DeliveryID, incidentID: row.IncidentID, organizationID: row.OrganizationID,
		environmentID: row.EnvironmentID, recipient: row.RecipientEmail, transition: row.Transition,
		leaseToken: row.LeaseToken, attempt: int(row.AttemptNumber)}
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
	queries := database.New(tx)
	rowsAffected, err := queries.AcceptNotificationDelivery(ctx, database.AcceptNotificationDeliveryParams{
		AttemptNumber: int16(delivery.attempt), ProviderMessageID: notificationText(messageID),
		ProviderStatus: notificationText(status), AttemptedAt: notificationTimestamp(attemptedAt),
		DeliveryID: delivery.deliveryID, LeaseToken: delivery.leaseToken,
	})
	if err != nil {
		return err
	}
	if rowsAffected != 1 {
		return ErrLeaseLost
	}
	if err = queries.InsertNotificationAttempt(ctx, database.InsertNotificationAttemptParams{DeliveryID: delivery.deliveryID, AttemptNumber: int16(delivery.attempt), Outcome: "accepted", ProviderStatus: status, AttemptedAt: notificationTimestamp(attemptedAt)}); err != nil {
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
	queries := database.New(tx)
	rowsAffected, err := queries.CompleteFailedNotificationAttempt(ctx, database.CompleteFailedNotificationAttemptParams{
		State: state, AttemptNumber: int16(delivery.attempt), NextAttemptAt: notificationTimestamp(nextAttempt),
		ProviderStatus: notificationText(status), AttemptedAt: notificationTimestamp(attemptedAt),
		DeliveryID: delivery.deliveryID, LeaseToken: delivery.leaseToken,
	})
	if err != nil {
		return err
	}
	if rowsAffected != 1 {
		return ErrLeaseLost
	}
	if err = queries.InsertNotificationAttempt(ctx, database.InsertNotificationAttemptParams{DeliveryID: delivery.deliveryID, AttemptNumber: int16(delivery.attempt), Outcome: outcome, ProviderStatus: status, AttemptedAt: notificationTimestamp(attemptedAt)}); err != nil {
		return err
	}
	if err = insertRefreshEvent(ctx, tx, delivery); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func insertRefreshEvent(ctx context.Context, tx pgx.Tx, delivery claimedDelivery) error {
	return database.New(tx).InsertNotificationRefreshEvent(ctx, database.InsertNotificationRefreshEventParams{OrganizationID: delivery.organizationID, EnvironmentID: delivery.environmentID, DeliveryID: delivery.deliveryID})
}

func notificationTimestamp(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
}

func notificationText(value string) pgtype.Text {
	return pgtype.Text{String: value, Valid: true}
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
