//go:build unit

package handler

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUpdateAPIKeyRequestGroupIDPreservesJSONThreeState(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantSet   bool
		wantValue *int64
	}{
		{name: "omitted", body: `{}`, wantSet: false},
		{name: "explicit null", body: `{"group_id":null}`, wantSet: true},
		{name: "number", body: `{"group_id":42}`, wantSet: true, wantValue: int64Pointer(42)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var req UpdateAPIKeyRequest
			require.NoError(t, json.Unmarshal([]byte(tc.body), &req))
			require.Equal(t, tc.wantSet, req.GroupID.Set)
			if tc.wantValue == nil {
				require.Nil(t, req.GroupID.Value)
				return
			}
			require.NotNil(t, req.GroupID.Value)
			require.Equal(t, *tc.wantValue, *req.GroupID.Value)
		})
	}
}

func int64Pointer(value int64) *int64 {
	return &value
}
