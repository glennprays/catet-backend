//go:build llmeval

// The live accuracy harness for receipt reading. It calls the real
// provider, so it is behind a build tag and never runs in CI:
//
//	set -a && . ./.env && set +a &&
//	go test -tags llmeval -run TestReceiptEval -v ./internal/llm/
//
// Adding a fixture = drop a JPEG in testdata/receipts and add one row to
// expected.json. Changing LLM_VISION_MODEL without running this is how a
// silently worse model ships.
package llm_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/glennprays/catet-backend/internal/llm"
)

const fixtureDir = "testdata/receipts"

// expectation is one scored fixture. Only the fields a photo can prove
// are checked: the printed total, whether the text was readable at all,
// and whether a TOTAL line was present.
type expectation struct {
	File       string `json:"file"`
	Merchant   string `json:"merchant"`
	TotalMinor int64  `json:"total_minor"`
	Date       string `json:"date"`
	Legible    bool   `json:"legible"`
	HasTotal   bool   `json:"has_total"`
	Currency   string `json:"currency"`
}

func TestReceiptEval(t *testing.T) {
	key := os.Getenv("LLM_API_KEY")
	if key == "" {
		t.Skip("LLM_API_KEY is empty: source .env to run the live accuracy harness")
	}
	base := os.Getenv("LLM_BASE_URL")
	if base == "" {
		t.Skip("LLM_BASE_URL is empty")
	}
	model := os.Getenv("LLM_MODEL")
	vision := os.Getenv("LLM_VISION_MODEL")
	timeout := 45 * time.Second
	if secs, err := strconv.Atoi(os.Getenv("LLM_VISION_TIMEOUT_SECONDS")); err == nil && secs > 0 {
		timeout = time.Duration(secs) * time.Second
	}

	raw, err := os.ReadFile(filepath.Join(fixtureDir, "expected.json"))
	if err != nil {
		t.Fatalf("read expectations: %v", err)
	}
	var want []expectation
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("decode expectations: %v", err)
	}

	client := llm.New(base, key, model, 20*time.Second).WithVision(vision, timeout)
	effective := vision
	if effective == "" {
		effective = model
	}
	t.Logf("model=%s timeout=%s fixtures=%d", effective, timeout, len(want))
	t.Logf("%-16s %-8s %-10s %-10s %-22s %-12s %8s %12s",
		"file", "legible", "has_total", "total", "merchant", "date", "ms", "tokens")

	for _, exp := range want {
		img, err := os.ReadFile(filepath.Join(fixtureDir, exp.File))
		if err != nil {
			t.Fatalf("read fixture %s: %v", exp.File, err)
		}

		started := time.Now()
		read, err := client.ReadReceipt(context.Background(), img, mimeOf(exp.File), "", nil)
		elapsed := time.Since(started)
		if err != nil {
			t.Errorf("%s: ReadReceipt failed: %v", exp.File, err)
			continue
		}

		t.Logf("%-16s %-8t %-10t %-10d %-22s %-12s %8d %12s",
			exp.File, read.Legible, read.HasTotalLine, read.TotalMinor,
			truncate(read.Merchant, 22), read.Date, elapsed.Milliseconds(),
			strconv.Itoa(read.PromptTokens)+"/"+strconv.Itoa(read.CompletionTokens))

		// An illegible photo must never come back with a number: a
		// guessed total is worse than a refusal.
		if !exp.Legible {
			if read.Legible && read.TotalMinor != 0 {
				t.Errorf("%s: illegible fixture read as legible with total %d", exp.File, read.TotalMinor)
			}
			continue
		}
		if !read.Legible {
			t.Errorf("%s: legible fixture refused, issues=%v", exp.File, read.Issues)
			continue
		}
		// A receipt with no visible TOTAL line must report no total, not
		// the sum of the items it could see.
		if !exp.HasTotal {
			if read.HasTotalLine || read.TotalMinor != 0 {
				t.Errorf("%s: cropped fixture reported total %d (has_total_line=%t)",
					exp.File, read.TotalMinor, read.HasTotalLine)
			}
			continue
		}
		if read.TotalMinor != exp.TotalMinor {
			t.Errorf("%s: total_minor = %d, want %d", exp.File, read.TotalMinor, exp.TotalMinor)
		}
		if exp.Merchant != "" && !strings.HasPrefix(
			strings.ToUpper(read.Merchant), strings.ToUpper(exp.Merchant)) {
			t.Errorf("%s: merchant = %q, want prefix %q", exp.File, read.Merchant, exp.Merchant)
		}
		if exp.Date != "" && read.Date != exp.Date {
			t.Errorf("%s: date = %q, want %q", exp.File, read.Date, exp.Date)
		}
	}
}

func mimeOf(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	default:
		return "image/jpeg"
	}
}

func truncate(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}
