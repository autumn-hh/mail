package web_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthReportsClosedDatabase(t *testing.T) {
	fixture := newPublicFixture(t)
	handler := fixture.handler
	for _, closed := range []bool{false, true} {
		if closed {
			if err := fixture.db.Close(); err != nil {
				t.Fatal(err)
			}
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", "/healthz", nil))
		want := http.StatusOK
		if closed {
			want = http.StatusServiceUnavailable
		}
		if response.Code != want {
			t.Fatalf("closed=%v status=%d", closed, response.Code)
		}
	}
}
