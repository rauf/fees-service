package domain

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePeriod(t *testing.T) {
	tests := map[string]struct {
		in        string
		wantStart time.Time
		wantErr   bool
	}{
		"month":        {in: "2026-11", wantStart: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)},
		"single digit": {in: "2026-1", wantErr: true},
		"month 13":     {in: "2026-13", wantErr: true},
		"with day":     {in: "2026-11-01", wantErr: true},
		"empty":        {in: "", wantErr: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := ParsePeriod(tc.in)
			if tc.wantErr {
				require.ErrorIs(t, err, ErrInvalidPeriod)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.wantStart, got.Start())
			assert.Equal(t, tc.in, got.String())
		})
	}
}

func TestPeriodBounds(t *testing.T) {
	dec := mustParse(t, "2026-12")

	tests := map[string]struct {
		at   time.Time
		want bool
	}{
		"first instant":      {at: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), want: true},
		"last instant":       {at: time.Date(2026, 12, 31, 23, 59, 59, 999, time.UTC), want: true},
		"next month":         {at: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), want: false},
		"previous month":     {at: time.Date(2026, 11, 30, 23, 59, 59, 0, time.UTC), want: false},
		"same instant in +4": {at: time.Date(2027, 1, 1, 3, 0, 0, 0, time.FixedZone("GET", 4*3600)), want: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, dec.Contains(tc.at))
		})
	}

	assert.Equal(t, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), dec.End(), "December ends at the next year")
}

func TestPeriodJSON(t *testing.T) {
	// The shape Temporal history already holds: a bill started before Period existed must decode.
	var bill Bill
	require.NoError(t, json.Unmarshal([]byte(`{"period":"2026-11"}`), &bill))
	assert.Equal(t, mustParse(t, "2026-11"), bill.Period)

	out, err := json.Marshal(Bill{Period: mustParse(t, "2026-11")})
	require.NoError(t, err)
	assert.Contains(t, string(out), `"period":"2026-11"`)

	var zero Bill
	require.NoError(t, json.Unmarshal([]byte(`{"period":""}`), &zero))
	assert.True(t, zero.Period.IsZero())

	require.Error(t, json.Unmarshal([]byte(`{"period":"2026-13"}`), &zero))
}

func mustParse(t *testing.T, s string) Period {
	t.Helper()

	p, err := ParsePeriod(s)
	require.NoError(t, err)

	return p
}
