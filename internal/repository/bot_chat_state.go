package repository

import (
	"context"
	"errors"
	"time"

	"github.com/glennprays/catet-backend/internal/scope"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ChatState is the bot's per-chat memory: the batch `batal` addresses
// and the single outstanding clarifying question.
type ChatState struct {
	Chat string
	// UserID is the chat's owner, filled on read. Writes take the owner
	// from the scope instead, so it is never a way to address another
	// tenant's row.
	UserID              uuid.UUID
	LastBatchMessageID  *string
	OpenPendingActionID *uuid.UUID
	OpenExpect          *string
	UpdatedAt           time.Time
}

type BotChatStateRepository struct {
	db *pgxpool.Pool
}

func NewBotChatStateRepository(db *pgxpool.Pool) *BotChatStateRepository {
	return &BotChatStateRepository{db: db}
}

// Get returns the chat's state; nil when the chat has none yet.
func (r *BotChatStateRepository) Get(ctx context.Context, chat string) (*ChatState, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	var s ChatState
	err = r.db.QueryRow(ctx,
		`SELECT chat, user_id, last_batch_message_id, open_pending_action_id, open_expect, updated_at
		 FROM bot_chat_state WHERE chat = $1 AND user_id = $2`, chat, uid,
	).Scan(&s.Chat, &s.UserID, &s.LastBatchMessageID, &s.OpenPendingActionID, &s.OpenExpect, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// Upsert replaces the whole state row — the caller always holds the
// complete intended state, so a partial update would hide staleness.
// The owner comes from the scope, never from the struct: a chat belongs
// to whoever the message was resolved to.
func (r *BotChatStateRepository) Upsert(ctx context.Context, s *ChatState) error {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return err
	}
	_, err = r.db.Exec(ctx,
		`INSERT INTO bot_chat_state (chat, user_id, last_batch_message_id, open_pending_action_id, open_expect, updated_at)
		 VALUES ($1,$2,$3,$4,$5,now())
		 ON CONFLICT (chat) DO UPDATE SET
		   user_id = EXCLUDED.user_id,
		   last_batch_message_id = EXCLUDED.last_batch_message_id,
		   open_pending_action_id = EXCLUDED.open_pending_action_id,
		   open_expect = EXCLUDED.open_expect,
		   updated_at = now()`,
		s.Chat, uid, s.LastBatchMessageID, s.OpenPendingActionID, s.OpenExpect,
	)
	return err
}
