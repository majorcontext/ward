package wardapi_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/majorcontext/ward/wardapi"
)

// A land that failed and an error response carry the same error with a details key, as the specification says.
func TestErrorAlwaysHasDetails(t *testing.T) {
	t.Parallel()
	for _, v := range []any{wardapi.ErrorBody{Error: wardapi.Error{Code: wardapi.CodeNotFound}}, wardapi.Land{Error: &wardapi.Error{Code: wardapi.CodeLandStale}}} {
		raw, err := json.Marshal(v)
		if err != nil || !strings.Contains(string(raw), `"details":null`) {
			t.Errorf("%s, %v", raw, err)
		}
	}
}
