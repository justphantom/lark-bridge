package miniagent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchModels_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q, want /v1/models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("Authorization = %q, want Bearer sk-test", got)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o"},{"id":"gpt-4o-mini"}]}`))
	}))
	defer srv.Close()
	got, err := fetchModels(context.Background(), srv.URL, "sk-test")
	if err != nil {
		t.Fatalf("fetchModels: %v", err)
	}
	want := []string{"gpt-4o", "gpt-4o-mini"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("idx %d: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestFetchModels_EmptyData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	got, err := fetchModels(context.Background(), srv.URL, "sk-test")
	if err != nil {
		t.Fatalf("fetchModels on empty data: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want empty slice", got)
	}
}

func TestFetchModels_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	_, err := fetchModels(context.Background(), srv.URL, "sk-test")
	if err == nil {
		t.Fatal("expected error for 404, got nil")
	}
	// Error must hint the /model <id> fallback so the user knows what to do.
	if !strings.Contains(err.Error(), "/model") {
		t.Errorf("err = %q, want it to mention /model fallback", err.Error())
	}
}

func TestFetchModels_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	_, err := fetchModels(context.Background(), srv.URL, "sk-test")
	if err == nil {
		t.Fatal("expected error for 500, got nil")
	}
}

func TestFetchModels_InvalidBaseURL(t *testing.T) {
	_, err := fetchModels(context.Background(), "ht!tp://broken", "sk-test")
	if err == nil {
		t.Fatal("expected error for invalid base_url")
	}
}
