package money_test

import (
	"testing"

	"github.com/glennprays/catet-backend/internal/money"
	"github.com/stretchr/testify/assert"
)

func TestFormatIDR(t *testing.T) {
	cases := []struct {
		minor int64
		want  string
	}{
		{0, "Rp0"},
		{500, "Rp500"},
		{25_000, "Rp25.000"},
		{8_003_500, "Rp8.003.500"},
		{-685_000, "\u2212Rp685.000"},
		{1_000_000_000, "Rp1.000.000.000"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, money.FormatIDR(c.minor), "FormatIDR(%d)", c.minor)
	}
}

func TestFormatSigned(t *testing.T) {
	assert.Equal(t, "+Rp8.000.000", money.FormatSigned(8_000_000))
	assert.Equal(t, "\u2212Rp25.000", money.FormatSigned(-25_000))
	assert.Equal(t, "Rp0", money.FormatSigned(0))
}

func TestParseToken(t *testing.T) {
	ok := []struct {
		in   string
		want int64
	}{
		{"25rb", 25_000},
		{"25k", 25_000},
		{"25K", 25_000},
		{"2,5jt", 2_500_000},
		{"2.5jt", 2_500_000},
		{"1.250.000", 1_250_000},
		{"25000", 25_000},
		{"Rp25.000", 25_000},
		{"rp 25.000", 25_000},
		{"500 ribu", 500_000},
		{"2 juta", 2_000_000},
		{"18rb", 18_000},
		{"1,5juta", 1_500_000},
		{"3m", 3_000_000},
	}
	for _, c := range ok {
		got, valid := money.ParseToken(c.in)
		if assert.True(t, valid, "ParseToken(%q) should parse", c.in) {
			assert.Equal(t, c.want, got, "ParseToken(%q)", c.in)
		}
	}

	bad := []string{
		"",
		"rp",
		"abc",
		"25.000,50", // sub-rupiah cents do not exist
		"0",
		"-25000",
		"99999999999999999",
		"25rbx",
	}
	for _, in := range bad {
		_, valid := money.ParseToken(in)
		assert.False(t, valid, "ParseToken(%q) should be refused", in)
	}
}
