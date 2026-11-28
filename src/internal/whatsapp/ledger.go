package whatsapp

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ItzTas/monitor-hub/src/internal/db"
)

// Ledger remembers which recipients already got each event, so a retry skips
// them. Reached is keyed by recipient.
type Ledger interface {
	Reached(ctx context.Context, seqs []int64) (map[string]map[int64]bool, error)
	Record(ctx context.Context, recipient string, seqs []int64) error
}

type ledger struct {
	queries *db.Queries
}

func NewLedger(pool *pgxpool.Pool) Ledger {
	return ledger{queries: db.New(pool)}
}

func (l ledger) Reached(ctx context.Context, seqs []int64) (map[string]map[int64]bool, error) {
	rows, err := l.queries.ReachedRecipients(ctx, db.ReachedRecipientsParams{Channel: channelName, Seqs: seqs})
	if err != nil {
		return nil, err
	}
	reached := map[string]map[int64]bool{}
	for _, row := range rows {
		if reached[row.Recipient] == nil {
			reached[row.Recipient] = map[int64]bool{}
		}
		reached[row.Recipient][row.EventSeq] = true
	}
	return reached, nil
}

func (l ledger) Record(ctx context.Context, recipient string, seqs []int64) error {
	return l.queries.RecordRecipient(ctx, db.RecordRecipientParams{Seqs: seqs, Channel: channelName, Recipient: recipient})
}
