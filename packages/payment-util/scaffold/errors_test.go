package scaffold

import "testing"

func TestHttpStatusTitle(t *testing.T) {
	cases := map[int]string{
		400: "Bad Request", 401: "Unauthorized", 403: "Forbidden",
		404: "Not Found", 409: "Conflict", 422: "Unprocessable Entity",
		429: "Too Many Requests", 500: "Internal Server Error",
		503: "Service Unavailable", 999: "HTTP Error",
	}
	for s, want := range cases {
		if got := httpStatusTitle(s); got != want {
			t.Errorf("title(%d) = %q, want %q", s, got, want)
		}
	}
}

func TestHttpStatusCode(t *testing.T) {
	if got := httpStatusCode(404); got != "not_found" {
		t.Errorf("404 = %q", got)
	}
	if got := httpStatusCode(500); got != "internal_error" {
		t.Errorf("500 = %q", got)
	}
	if got := httpStatusCode(999); got != "http_error" {
		t.Errorf("999 = %q", got)
	}
}
