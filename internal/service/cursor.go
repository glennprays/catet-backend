// Opaque keyset cursors: base64url("v1|<unixnano>|<uuid>").
package service

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/google/uuid"
)

func encodeCursorValue(raw string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursorValue(s string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		// Tolerate padded input.
		b, err = base64.URLEncoding.DecodeString(s)
		if err != nil {
			return "", fmt.Errorf("malformed cursor: %w", err)
		}
	}
	return string(b), nil
}

func decodeCursor(s string) (*repository.Cursor, error) {
	raw, err := decodeCursorValue(s)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(raw, "|")
	if len(parts) != 3 || parts[0] != "v1" {
		return nil, fmt.Errorf("malformed cursor payload")
	}
	atNano, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("malformed cursor timestamp: %w", err)
	}
	id, err := uuid.Parse(parts[2])
	if err != nil {
		return nil, fmt.Errorf("malformed cursor id: %w", err)
	}
	return &repository.Cursor{At: time.Unix(0, atNano).UTC(), ID: id}, nil
}
