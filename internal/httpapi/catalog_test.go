package httpapi_test

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/ottermq/otter_cloud/internal/catalog"
	"github.com/ottermq/otter_cloud/internal/dbtest"
)

// signedUp is a browser logged in as a new user.
func (e *env) signedUp(email string) *http.Client {
	e.t.Helper()
	browser := e.client()
	resp, body := e.do(browser, "POST", "/api/signup",
		`{"token":"`+e.invite(email)+`","password":"`+goodPassword+`","account_name":"Lab"}`)
	if resp.StatusCode != http.StatusCreated {
		e.t.Fatalf("signup: %d %v", resp.StatusCode, body)
	}
	return browser
}

func TestCatalog(t *testing.T) {
	e := newEnv(t)

	resp, _ := e.do(e.client(), "GET", "/api/catalog", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("without a session: %d, want 401", resp.StatusCode)
	}

	resp, body := e.do(e.signedUp("ana@example.com"), "GET", "/api/catalog", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("catalog: %d %v", resp.StatusCode, body)
	}
	want := map[string]any{"service_types": []any{map[string]any{
		"code": "postgresql", "name": "PostgreSQL",
		"flavors": []any{
			map[string]any{"code": "db-small", "service_type": "postgresql", "name": "Small", "storage_gb": 2.0, "max_connections": 20.0},
			map[string]any{"code": "db-medium", "service_type": "postgresql", "name": "Medium", "storage_gb": 5.0, "max_connections": 30.0},
		},
	}}}
	if !reflect.DeepEqual(body, want) {
		t.Errorf("catalog = %v\nwant      %v", body, want)
	}
}

func TestCatalogLeavesOutInactiveFlavors(t *testing.T) {
	e := newEnv(t)
	// Drop every flavor from the file: all are deactivated, and a service
	// type with nothing to choose disappears.
	if _, err := catalog.Sync(context.Background(), e.db.ConnAs(dbtest.RoleMigrator), nil); err != nil {
		t.Fatal(err)
	}
	resp, body := e.do(e.signedUp("ana@example.com"), "GET", "/api/catalog", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("catalog: %d %v", resp.StatusCode, body)
	}
	if types, _ := body["service_types"].([]any); types == nil || len(types) != 0 {
		t.Errorf("service_types = %v, want an empty list", body["service_types"])
	}
}
