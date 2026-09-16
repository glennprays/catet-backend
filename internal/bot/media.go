package bot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	waga "github.com/glennprays/whatsapp-gateway-sdk-go"
)

const (
	// maxPhotoBytes bounds what a vision call may cost. WhatsApp's own
	// "regular photo" compression lands far below this; anything larger
	// was sent as HD or as a document.
	maxPhotoBytes = 6 << 20
	// mediaFetchTimeout bounds the download alone. The gateway serves
	// media from local storage, so anything slower is broken.
	mediaFetchTimeout = 20 * time.Second
)

var (
	// errMediaUnreadable covers both halves of "these bytes are not a
	// photo we can read": a failed download and, crucially, the
	// still-encrypted ciphertext WhatsApp's CDN returns when the gateway
	// did not download and re-host the media itself.
	errMediaUnreadable = errors.New("media unreadable")
	errMediaTooLarge   = errors.New("media too large")
	errMediaType       = errors.New("media type unsupported")
)

// photoMimes is the whitelist of container formats the vision models
// accept. A HEIC or a PDF is refused before anything is downloaded.
var photoMimes = map[string]bool{
	"image/jpeg": true,
	"image/jpg":  true,
	"image/png":  true,
	"image/webp": true,
}

// fetchPhoto downloads one inbound photo and returns its bytes, the
// sniffed mime type and the sha256 of the bytes. The sniff is the real
// gate: a gateway with media download disabled hands us an
// mmg.whatsapp.net URL whose body is E2E-encrypted, which sniffs as
// application/octet-stream rather than an image.
func fetchPhoto(ctx context.Context, client *http.Client, m *waga.IncomingMessageMediaInfo) ([]byte, string, string, error) {
	if m == nil || strings.TrimSpace(m.Url) == "" {
		return nil, "", "", errMediaUnreadable
	}
	if declared := strings.ToLower(strings.TrimSpace(m.MimeType)); declared != "" && !photoMimes[declared] {
		return nil, "", "", errMediaType
	}
	if m.Size > maxPhotoBytes {
		return nil, "", "", errMediaTooLarge
	}

	ctx, cancel := context.WithTimeout(ctx, mediaFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.Url, nil)
	if err != nil {
		return nil, "", "", fmt.Errorf("build media request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", "", fmt.Errorf("%w: %v", errMediaUnreadable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", "", fmt.Errorf("%w: status %d", errMediaUnreadable, resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPhotoBytes+1))
	if err != nil {
		return nil, "", "", fmt.Errorf("%w: %v", errMediaUnreadable, err)
	}
	if len(data) > maxPhotoBytes {
		return nil, "", "", errMediaTooLarge
	}

	mime := http.DetectContentType(data)
	if !strings.HasPrefix(mime, "image/") {
		return nil, "", "", fmt.Errorf("%w: sniffed %s", errMediaUnreadable, mime)
	}

	sum := sha256.Sum256(data)
	return data, mime, hex.EncodeToString(sum[:]), nil
}
