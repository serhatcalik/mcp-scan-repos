package notion

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidationDiagnosticsRetainFieldsNotSecrets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": "validation_error", "message": `body failed validation: body.children[0].code.language should be valid, instead was "secret-test-token glpat-private-value". body.children[0].code.language failed. body.secret_credentials must-not-log.`})
	}))
	defer server.Close()
	c := NewClient("secret-test-token")
	c.endpoint = server.URL
	err := c.request(context.Background(), http.MethodPatch, "/blocks/"+testParent+"/children", map[string]any{"children": []block{}}, nil)
	var failure *APIError
	if !errors.As(err, &failure) || failure.Status != 400 || failure.Operation != "append_blocks" || failure.Code != "validation_error" || len(failure.Fields) != 1 || failure.Fields[0] != "body.children[0].code.language" {
		t.Fatalf("missing diagnostics: %v", err)
	}
	for _, secret := range []string{"secret-test-token", "glpat-", "secret_credentials", "must-not-log"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("remote content exposed: %s", secret)
		}
	}
}

func TestErrorCodesAndOperationNamesAreNotArbitraryRemoteValues(t *testing.T) {
	if safeErrorCode("secret-test-token") != "" || requestOperation(http.MethodGet, "/pages/secret-test-token") != "read_page" {
		t.Fatal("arbitrary value became diagnostic")
	}
	fields := validationFields("body.children[0].text.content body.children[1].text.content body.annotations.glpat_secret_value " + strings.Repeat("body.children[2].code ", 20))
	if len(fields) != 3 {
		t.Fatalf("unknown schema names or duplicates leaked: %v", fields)
	}
}
