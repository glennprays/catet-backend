package service_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/llm"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/glennprays/catet-backend/internal/testkit"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeMerchantLLM answers merchant_pick prompts from a script and counts
// the calls, which is how these tests prove alias memory stops the model
// from ever being asked twice about the same phrase.
type fakeMerchantLLM struct {
	mu      sync.Mutex
	calls   int
	status  int
	answer  func(raw string) string
	prompts []string
}

func newFakeMerchantLLM(t *testing.T, answer func(raw string) string) (*fakeMerchantLLM, *llm.Client) {
	t.Helper()
	f := &fakeMerchantLLM{answer: answer}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Len(t, body.Messages, 2)

		f.mu.Lock()
		f.calls++
		f.prompts = append(f.prompts, body.Messages[1].Content)
		status, answer := f.status, f.answer
		f.mu.Unlock()

		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
			return
		}
		raw := strings.TrimPrefix(strings.SplitN(body.Messages[1].Content, "\n", 2)[0], "Raw: ")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": answer(raw)}}},
		})
	}))
	t.Cleanup(srv.Close)
	return f, llm.New(srv.URL, "test-key", "test-model", 2*time.Second)
}

func (f *fakeMerchantLLM) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeMerchantLLM) fail(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
}

type merchantFixture struct {
	ctx   context.Context
	pool  *pgxpool.Pool
	svc   *service.MerchantService
	repo  *repository.MerchantRepository
	model *fakeMerchantLLM
}

func newMerchantFixture(t *testing.T, phone string, answer func(raw string) string) *merchantFixture {
	t.Helper()
	pool := testkit.NewDB(t)
	ctx, _ := testkit.Scoped(t, pool, phone, "Merchants")
	model, client := newFakeMerchantLLM(t, answer)
	repo := repository.NewMerchantRepository(pool)
	return &merchantFixture{
		ctx:   ctx,
		pool:  pool,
		svc:   service.NewMerchantService(repo, client, testkit.Logger(t)),
		repo:  repo,
		model: model,
	}
}

// aliasSource reads how a phrase came to be remembered.
func (f *merchantFixture) aliasSource(t *testing.T, key string) string {
	t.Helper()
	var src string
	require.NoError(t, f.pool.QueryRow(f.ctx,
		`SELECT source FROM merchant_aliases WHERE alias_key = $1`, key).Scan(&src))
	return src
}

func newName(name string) func(string) string {
	return func(string) string {
		return `{"merchant_id":null,"name":"` + name + `","confidence":0.9}`
	}
}

// TestMerchantAliasMemorySkipsTheModel: the second spelling costs one
// model call, the repeat of either costs none.
func TestMerchantAliasMemorySkipsTheModel(t *testing.T) {
	f := newMerchantFixture(t, "+628111000101", newName("Traktir Teman"))

	first, err := f.svc.Resolve(f.ctx, "traktir makan temen", "")
	require.NoError(t, err)
	require.NotNil(t, first)
	assert.Equal(t, "Traktir Teman", first.Name)
	assert.Equal(t, 1, f.model.callCount())
	assert.Equal(t, "llm", f.aliasSource(t, "traktir makan temen"))

	again, err := f.svc.Resolve(f.ctx, "Traktir Makan Temen", "")
	require.NoError(t, err)
	require.NotNil(t, again)
	assert.Equal(t, first.ID, again.ID)
	assert.Equal(t, 1, f.model.callCount(), "a remembered phrase never reaches the model")

	// The canonical name itself is a deterministic hit, recorded as a
	// rules alias rather than a second model call.
	byName, err := f.svc.Resolve(f.ctx, "traktir teman", "")
	require.NoError(t, err)
	require.NotNil(t, byName)
	assert.Equal(t, first.ID, byName.ID)
	assert.Equal(t, 1, f.model.callCount())
	assert.Equal(t, "rules", f.aliasSource(t, "traktir teman"))
}

// TestMerchantModelMatchNeedsConfidence: a confident match folds into the
// existing merchant; a hesitant one gets its own row instead, because a
// wrong merge is harder to notice than a duplicate.
func TestMerchantModelMatchNeedsConfidence(t *testing.T) {
	f := newMerchantFixture(t, "+628111000102", newName("Kopi Kenangan"))

	base, err := f.svc.Resolve(f.ctx, "kopi kenangan", "")
	require.NoError(t, err)
	require.NotNil(t, base)

	confident := func(string) string {
		return `{"merchant_id":"` + base.ID.String() + `","name":"Kopi Kenangan","confidence":0.9}`
	}
	f.model.mu.Lock()
	f.model.answer = confident
	f.model.mu.Unlock()

	hit, err := f.svc.Resolve(f.ctx, "kopi kenangan tp", "")
	require.NoError(t, err)
	require.NotNil(t, hit)
	assert.Equal(t, base.ID, hit.ID)
	assert.Equal(t, "llm", f.aliasSource(t, "kopi kenangan tp"))

	hesitant := func(string) string {
		return `{"merchant_id":"` + base.ID.String() + `","name":"Kopi Lain","confidence":0.6}`
	}
	f.model.mu.Lock()
	f.model.answer = hesitant
	f.model.mu.Unlock()

	other, err := f.svc.Resolve(f.ctx, "kopi lain sekali", "")
	require.NoError(t, err)
	require.NotNil(t, other)
	assert.NotEqual(t, base.ID, other.ID)
	assert.Equal(t, "Kopi Lain", other.Name)
}

// TestMerchantModelIDOutsideWhitelistIsDropped mirrors ADR-0003: an
// invented id is not a match, the tidied name still stands.
func TestMerchantModelIDOutsideWhitelistIsDropped(t *testing.T) {
	invented := uuid.New().String()
	f := newMerchantFixture(t, "+628111000103", func(string) string {
		return `{"merchant_id":"` + invented + `","name":"Indomaret","confidence":0.99}`
	})

	m, err := f.svc.Resolve(f.ctx, "INDOMARET TUNJUNGAN 2", "")
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.NotEqual(t, invented, m.ID.String())
	assert.Equal(t, "Indomaret", m.Name)
}

// TestMerchantResolveDegradesToTheTypedPhrase: the model being down must
// never block a capture — the phrase is kept, Title-Cased, as a raw
// alias the user can merge later.
func TestMerchantResolveDegradesToTheTypedPhrase(t *testing.T) {
	f := newMerchantFixture(t, "+628111000104", newName("Never Used"))
	f.model.fail(http.StatusInternalServerError)

	m, err := f.svc.Resolve(f.ctx, "warung mbak yu", "")
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Equal(t, "Warung Mbak Yu", m.Name)
	assert.Equal(t, "raw", f.aliasSource(t, "warung mbak yu"))
}

// TestResolveUserNeverAsksTheModel: text the user chose on purpose is
// authoritative as typed. Acronyms stay as typed.
func TestResolveUserNeverAsksTheModel(t *testing.T) {
	f := newMerchantFixture(t, "+628111000105", newName("Never Used"))

	m, err := f.svc.ResolveUser(f.ctx, "kopi kenangan")
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Equal(t, "Kopi Kenangan", m.Name)
	assert.Equal(t, 0, f.model.callCount())
	assert.Equal(t, "user", f.aliasSource(t, "kopi kenangan"))

	acronym, err := f.svc.ResolveUser(f.ctx, "KFC cabang baru")
	require.NoError(t, err)
	require.NotNil(t, acronym)
	assert.Equal(t, "KFC Cabang Baru", acronym.Name)
}

// TestMerchantLookupIsMemoryOnly: the refund path must not invent a
// merchant just because the user typed an unknown word.
func TestMerchantLookupIsMemoryOnly(t *testing.T) {
	f := newMerchantFixture(t, "+628111000106", newName("Gojek"))

	miss, err := f.svc.Lookup(f.ctx, "gojek")
	require.NoError(t, err)
	assert.Nil(t, miss)
	assert.Equal(t, 0, f.model.callCount())

	created, err := f.svc.Resolve(f.ctx, "gojek", "")
	require.NoError(t, err)
	require.NotNil(t, created)

	hit, err := f.svc.Lookup(f.ctx, "Gojek")
	require.NoError(t, err)
	require.NotNil(t, hit)
	assert.Equal(t, created.ID, hit.ID)
}

// TestMerchantRenameRewritesRows: the ledger's denormalised display text
// follows the canonical name.
func TestMerchantRenameRewritesRows(t *testing.T) {
	f := newMerchantFixture(t, "+628111000107", newName("Traktir Teman"))
	txs := newMerchantLedger(t, f)

	m, err := f.svc.Resolve(f.ctx, "traktir makan temen", "")
	require.NoError(t, err)
	tx := txs.expense(t, m.ID, 50_000)

	renamed, err := f.svc.Rename(f.ctx, m.ID, "Traktir")
	require.NoError(t, err)
	assert.Equal(t, "Traktir", renamed.Name)

	var merchant string
	require.NoError(t, f.pool.QueryRow(f.ctx,
		`SELECT merchant FROM transactions WHERE id = $1`, tx).Scan(&merchant))
	assert.Equal(t, "Traktir", merchant)

	// The new name is remembered too, so typing it resolves without a
	// model call.
	back, err := f.svc.Lookup(f.ctx, "traktir")
	require.NoError(t, err)
	require.NotNil(t, back)
	assert.Equal(t, m.ID, back.ID)
}

// TestMerchantMergeMovesEverything: rows, aliases and the old name all
// follow the survivor; the source row disappears.
func TestMerchantMergeMovesEverything(t *testing.T) {
	f := newMerchantFixture(t, "+628111000108", func(raw string) string {
		return `{"merchant_id":null,"name":"` + strings.ToUpper(raw[:1]) + raw[1:] + `","confidence":0.9}`
	})
	txs := newMerchantLedger(t, f)

	from, err := f.svc.Resolve(f.ctx, "kopi kenangan tp", "")
	require.NoError(t, err)
	into, err := f.svc.Resolve(f.ctx, "kopi kenangan", "")
	require.NoError(t, err)
	require.NotEqual(t, from.ID, into.ID)

	tx := txs.expense(t, from.ID, 25_000)

	require.NoError(t, f.svc.Merge(f.ctx, from.ID, into.ID))

	var merchantID uuid.UUID
	var merchant string
	require.NoError(t, f.pool.QueryRow(f.ctx,
		`SELECT merchant_id, merchant FROM transactions WHERE id = $1`, tx).Scan(&merchantID, &merchant))
	assert.Equal(t, into.ID, merchantID)
	assert.Equal(t, into.Name, merchant)

	gone, err := f.repo.Get(f.ctx, from.ID)
	require.NoError(t, err)
	assert.Nil(t, gone)

	// Both the merged merchant's own name and its aliases now land on
	// the survivor.
	byOldName, err := f.svc.Lookup(f.ctx, from.Name)
	require.NoError(t, err)
	require.NotNil(t, byOldName)
	assert.Equal(t, into.ID, byOldName.ID)

	byOldAlias, err := f.svc.Lookup(f.ctx, "kopi kenangan tp")
	require.NoError(t, err)
	require.NotNil(t, byOldAlias)
	assert.Equal(t, into.ID, byOldAlias.ID)

	rows, err := f.svc.List(f.ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, into.ID, rows[0].ID)
	assert.Equal(t, int64(1), rows[0].TxCount)
	assert.Equal(t, int64(25_000), rows[0].TotalMinor)
}

// TestMerchantMergeIntoItselfIsRejected keeps a no-op from deleting the
// merchant it was supposed to keep.
func TestMerchantMergeIntoItselfIsRejected(t *testing.T) {
	f := newMerchantFixture(t, "+628111000109", newName("Indomaret"))

	m, err := f.svc.Resolve(f.ctx, "indomaret", "")
	require.NoError(t, err)
	require.Error(t, f.svc.Merge(f.ctx, m.ID, m.ID))
}

// merchantLedger writes minimal expenses pointing at a merchant, so the
// rename/merge tests have rows to move.
type merchantLedger struct {
	repo   *repository.TransactionRepository
	source uuid.UUID
	ctx    context.Context
}

func newMerchantLedger(t *testing.T, f *merchantFixture) *merchantLedger {
	t.Helper()
	src, err := repository.NewSourceRepository(f.pool).Create(f.ctx, &entity.Source{
		ID: uuid.New(), Name: "Cash", Type: "cash", Currency: "IDR",
	})
	require.NoError(t, err)
	return &merchantLedger{repo: repository.NewTransactionRepository(f.pool), source: src.ID, ctx: f.ctx}
}

func (l *merchantLedger) expense(t *testing.T, merchantID uuid.UUID, amount int64) uuid.UUID {
	t.Helper()
	name := "seed"
	tx, err := l.repo.Insert(l.ctx, &entity.Transaction{
		ID:           uuid.New(),
		OccurredAt:   time.Now().UTC(),
		Origin:       "dashboard_manual",
		Type:         "expense",
		Merchant:     &name,
		MerchantID:   &merchantID,
		ReviewStatus: "needs_review",
		Legs: []entity.Leg{
			{ID: uuid.New(), SourceID: &l.source, SignedAmount: -amount},
			{ID: uuid.New(), SignedAmount: amount},
		},
	})
	require.NoError(t, err)
	return tx.ID
}
