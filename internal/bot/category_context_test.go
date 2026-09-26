package bot

import (
	"context"
	"testing"
	"time"

	"github.com/glennprays/catet-backend/internal/llm"
	"github.com/stretchr/testify/require"
)

func TestCategoryModelContextCapsAndReservesCoreDeadline(t *testing.T) {
	h := &Harness{llm: llm.New("https://provider.example", "key", "model", 30*time.Second)}

	model, modelCancel, ok := h.categoryModelContext(context.Background())
	require.True(t, ok)
	require.NotNil(t, model)
	cappedDeadline, hasDeadline := model.Deadline()
	require.True(t, hasDeadline)
	require.WithinDuration(t, time.Now().Add(categoryModelMaxTimeout), cappedDeadline, 100*time.Millisecond)
	modelCancel()

	deadline := time.Now().Add(10 * time.Second)
	core, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	model, modelCancel, ok = h.categoryModelContext(core)
	require.True(t, ok)
	require.NotNil(t, model)
	defer modelCancel()

	modelDeadline, hasDeadline := model.Deadline()
	require.True(t, hasDeadline)
	require.WithinDuration(t, deadline.Add(-categoryWriteReserve), modelDeadline, 100*time.Millisecond)
}

// A photo spends most of its budget on the vision read before either
// enrichment caller runs, so the auxiliary budget must start when it is
// minted, not when the message arrived. It still refuses to start
// without enough room left for the write.
func TestAuxiliaryModelContextStartsFresh(t *testing.T) {
	h := &Harness{llm: llm.New("https://provider.example", "key", "model", 30*time.Second)}

	// The parent deadline is already well past the five-second budget —
	// a message that arrived long ago — but it still has work time left,
	// so the fresh budget is the full auxiliary timeout, not what is
	// left of the parent's.
	core, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	model, modelCancel, ok := h.auxiliaryModelContext(core)
	require.True(t, ok, "a context with work time left gets a fresh budget")
	require.NotNil(t, model)
	defer modelCancel()

	modelDeadline, hasDeadline := model.Deadline()
	require.True(t, hasDeadline)
	require.WithinDuration(t, time.Now().Add(auxiliaryEnrichmentTimeout), modelDeadline, 100*time.Millisecond,
		"the budget starts now, not when the message arrived")

	// Less than the write reserve left: no enrichment call at all.
	tight, tightCancel := context.WithTimeout(context.Background(), auxiliaryWriteReserve)
	defer tightCancel()
	model, modelCancel, ok = h.auxiliaryModelContext(tight)
	require.False(t, ok, "no budget may eat the write reserve")
	require.Nil(t, model)
	require.Nil(t, modelCancel)
}

func TestCategoryModelContextSkipsWithoutUsefulCoreTime(t *testing.T) {
	h := &Harness{llm: llm.New("https://provider.example", "key", "model", time.Second)}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	model, modelCancel, ok := h.categoryModelContext(canceled)
	require.False(t, ok)
	require.Nil(t, model)
	require.Nil(t, modelCancel)

	short, shortCancel := context.WithTimeout(context.Background(), categoryWriteReserve)
	defer shortCancel()
	model, modelCancel, ok = h.categoryModelContext(short)
	require.False(t, ok)
	require.Nil(t, model)
	require.Nil(t, modelCancel)
}
