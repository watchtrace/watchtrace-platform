package fifo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/watchtrace/watchtrace-platform/internal/envelope"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
)

type SendInput struct {
	QueueURL                 string
	Body                     []byte
	DeduplicationID, GroupID string
	Attributes               envelope.Attributes
}
type Sender interface {
	Send(context.Context, SendInput) (string, error)
}
type Publisher struct {
	db     DB
	sender Sender
	now    func() time.Time
}

func NewPublisher(db DB, s Sender) *Publisher { return &Publisher{db: db, sender: s, now: time.Now} }

type outbox struct {
	JobID, Pool, Queue, Dedup, Group string
	PlatformKeyID, WorkerKeyID       string
	Body, Hash                       []byte
	Expiry                           time.Time
	Attempts                         int16
	Schema                           int16
	Token                            string
}

func (p *Publisher) PublishNext(ctx context.Context) (bool, error) {
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.Background())
	claimed, err := database.New(tx).ClaimDispatchOutbox(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	row := outbox{JobID: claimed.JobID, Pool: claimed.WorkerPoolID, Queue: claimed.QueueUrl, Body: claimed.MessageBody, Hash: claimed.SnapshotHash, Dedup: claimed.MessageDeduplicationID, Group: claimed.MessageGroupID, Expiry: claimed.ExpiresAt.Time, Attempts: claimed.PublishAttempts, Schema: claimed.SchemaVersion, PlatformKeyID: claimed.PlatformKeyID, WorkerKeyID: claimed.WorkerEncryptionKeyID, Token: claimed.PublishLeaseToken}
	queries := database.New(tx)
	if p.now().UTC().After(row.Expiry) {
		err = queries.ExpireClaimedDispatch(ctx, database.ExpireClaimedDispatchParams{JobID: row.JobID, LeaseToken: row.Token})
		if err == nil {
			err = queries.ExpireUnpublishedCheckJob(ctx, row.JobID)
		}
		if err == nil {
			err = queries.InsertExpiredCoverageGapFromJob(ctx, row.JobID)
		}
		if err != nil {
			return true, err
		}
		return true, tx.Commit(ctx)
	}
	if err = tx.Commit(ctx); err != nil {
		return true, err
	}
	attrs := envelope.Attributes{SchemaVersion: int(row.Schema), JobID: row.JobID, WorkerPoolID: row.Pool, SnapshotHash: fmtHash(row.Hash), ExpiresAt: row.Expiry, PlatformKeyID: row.PlatformKeyID, WorkerEncryptionKeyID: row.WorkerKeyID}
	messageID, sendErr := p.sender.Send(ctx, SendInput{QueueURL: row.Queue, Body: row.Body, DeduplicationID: row.Dedup, GroupID: row.Group, Attributes: attrs})
	tx, err = p.db.Begin(ctx)
	if err != nil {
		return true, err
	}
	defer tx.Rollback(context.Background())
	queries = database.New(tx)
	if sendErr == nil {
		err = queries.MarkDispatchPublished(ctx, database.MarkDispatchPublishedParams{MessageID: databaseText(messageID), JobID: row.JobID, LeaseToken: row.Token})
		if err == nil {
			err = queries.MarkCheckJobPublished(ctx, database.MarkCheckJobPublishedParams{MessageID: databaseText(messageID), JobID: row.JobID})
		}
	} else {
		state := "pending"
		delay := 10
		if row.Attempts == 2 {
			delay = 20
		}
		if row.Attempts >= 3 {
			state = "ambiguous"
			delay = 120
		}
		err = queries.MarkDispatchPublishFailure(ctx, database.MarkDispatchPublishFailureParams{PublishState: state, DelaySeconds: int32(delay), JobID: row.JobID, LeaseToken: row.Token})
	}
	if err != nil {
		return true, err
	}
	if err = tx.Commit(ctx); err != nil {
		return true, err
	}
	return true, sendErr
}
func fmtHash(b []byte) string { return fmt.Sprintf("%x", b) }
