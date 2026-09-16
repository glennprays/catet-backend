package secretbox_test

import (
	"encoding/base64"
	"testing"

	"github.com/glennprays/catet-backend/internal/secretbox"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSealOpenRoundTrip(t *testing.T) {
	box := secretbox.New("instance-secret")

	enc, err := box.Seal("sk-live-abcdef123456")
	require.NoError(t, err)
	assert.NotContains(t, enc, "sk-live", "the plaintext must not survive in the column")

	got, err := box.Open(enc)
	require.NoError(t, err)
	assert.Equal(t, "sk-live-abcdef123456", got)
}

// The empty secret is the documented "AI off" state and must stay
// recognisable in the column without a decrypt.
func TestEmptyIsEmpty(t *testing.T) {
	box := secretbox.New("instance-secret")

	enc, err := box.Seal("")
	require.NoError(t, err)
	assert.Equal(t, "", enc)

	got, err := box.Open("")
	require.NoError(t, err)
	assert.Equal(t, "", got)
}

// GCM is authenticated: a column somebody edited by hand must fail, not
// decrypt to garbage a request would then send to a provider.
func TestTamperedCiphertextFails(t *testing.T) {
	box := secretbox.New("instance-secret")
	enc, err := box.Seal("sk-tampered")
	require.NoError(t, err)

	raw, err := base64.StdEncoding.DecodeString(enc)
	require.NoError(t, err)
	raw[len(raw)-1] ^= 0xff

	_, err = box.Open(base64.StdEncoding.EncodeToString(raw))
	require.Error(t, err)

	_, err = box.Open("not base64 at all !!")
	require.Error(t, err)

	_, err = box.Open(base64.StdEncoding.EncodeToString([]byte("short")))
	require.Error(t, err)
}

// Rotating AUTH_SECRET must invalidate the stored key rather than
// silently yield something else.
func TestDifferentSecretsDoNotInteroperate(t *testing.T) {
	enc, err := secretbox.New("secret-one").Seal("sk-one")
	require.NoError(t, err)

	_, err = secretbox.New("secret-two").Open(enc)
	require.Error(t, err)
}
