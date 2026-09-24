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
