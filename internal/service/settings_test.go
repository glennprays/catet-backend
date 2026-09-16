package service_test

import (
	"errors"
	"testing"

	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/glennprays/catet-backend/internal/testkit"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The receipt fallback Source is the one setting the bot writes to by
// itself, so the API has to agree with it: an id must be a live Source,
// and clearing it must put the bot back to asking.
func TestSettingsPhotoDefaultSource(t *testing.T) {
	pool := testkit.NewDB(t)
	ctx, _ := testkit.Scoped(t, pool, "+628111000002", "Settings")
	sourceRepo := repository.NewSourceRepository(pool)
	svc := service.NewSettingsService(repository.NewSettingsRepository(pool), sourceRepo)

	bca, err := sourceRepo.Create(ctx, &entity.Source{
		ID: uuid.New(), Name: "BCA", Type: "bank", Currency: "IDR",
	})
	require.NoError(t, err)

	got, err := svc.Get(ctx)
	require.NoError(t, err)
	assert.Nil(t, got.PhotoDefaultSourceID, "no fallback until one is set")
	assert.Nil(t, got.PhotoDefaultSourceName)

	got, err = svc.Update(ctx, 7, &bca.ID, nil)
	require.NoError(t, err)
	require.NotNil(t, got.PhotoDefaultSourceID)
	assert.Equal(t, bca.ID, *got.PhotoDefaultSourceID)
	require.NotNil(t, got.PhotoDefaultSourceName)
	assert.Equal(t, "BCA", *got.PhotoDefaultSourceName, "the dashboard shows a name, not a uuid")

	// The stored value survives a reload.
	got, err = svc.Get(ctx)
	require.NoError(t, err)
	require.NotNil(t, got.PhotoDefaultSourceID)
	assert.Equal(t, bca.ID, *got.PhotoDefaultSourceID)

	unknown := uuid.New()
	_, err = svc.Update(ctx, 7, &unknown, nil)
	var fieldErr *apperror.Error
	require.True(t, errors.As(err, &fieldErr), "an unknown Source must be a field error")
	assert.Equal(t, apperror.KindValidation, fieldErr.Kind)
	require.Len(t, fieldErr.Details, 1)
	assert.Equal(t, "photo_default_source_id", fieldErr.Details[0].Field)

	// The refused write changed nothing.
	got, err = svc.Get(ctx)
	require.NoError(t, err)
	require.NotNil(t, got.PhotoDefaultSourceID)
	assert.Equal(t, bca.ID, *got.PhotoDefaultSourceID)

	got, err = svc.Update(ctx, 7, nil, nil)
	require.NoError(t, err)
	assert.Nil(t, got.PhotoDefaultSourceID, "clearing it puts the bot back to asking")
	assert.Nil(t, got.PhotoDefaultSourceName)
}

// The reminder hour is the user's one control over the only message the
// bot sends unprompted (ADR-0006), so it must round-trip and it must
// survive a client that does not know about it.
func TestSettingsReminderHour(t *testing.T) {
	pool := testkit.NewDB(t)
	ctx, _ := testkit.Scoped(t, pool, "+628111000012", "Reminder hour")
	svc := service.NewSettingsService(repository.NewSettingsRepository(pool), repository.NewSourceRepository(pool))

	got, err := svc.Get(ctx)
	require.NoError(t, err)
	assert.Equal(t, 8, got.ReminderHour, "the migration default is 08:00 WIB")

	got, err = svc.Update(ctx, 7, nil, new(19))
	require.NoError(t, err)
	assert.Equal(t, 19, got.ReminderHour)

	got, err = svc.Update(ctx, 14, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 19, got.ReminderHour, "an omitted hour leaves the digest where the user put it")
}
