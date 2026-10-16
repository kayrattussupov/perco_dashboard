package main

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeRaw map[string]string

func (f fakeRaw) GetRaw(_ context.Context, p string) ([]byte, int, error) {
	if body, ok := f[p]; ok {
		return []byte(body), 200, nil
	}
	return []byte("not found"), 404, nil
}

const fakeSpec = `{"paths":{
 "/users/staff":{"post":{"summary":"Массовое редактирование сотрудников","requestBody":{"content":{"application/json":{"schema":{"properties":{"is_block":{"type":"boolean"},"block_end_date":{"type":"string"}}}}}}}},
 "/planner":{"get":{"summary":"Записи планировщика","parameters":[{"name":"end_date","in":"query"}]}},
 "/devices/state":{"get":{"summary":"Состояние устройств"}}}}`

func TestDiscoverFollowsSwaggerUIAndHidesPersonalData(t *testing.T) {
	dir := t.TempDir()
	g := fakeRaw{
		"/api-docs":              `<html><script>SwaggerUIBundle({url: "swagger.json"})</script></html>`,
		"/api-docs/swagger.json": fakeSpec,
		"/api/users/staff/960":   `{"id":960,"last_name":"Раббимова","photo":"BASE64DATA","is_block":0,"block_date":"2026-10-12"}`,
	}
	var sb strings.Builder
	discover(context.Background(), g, 960, dir, log.New(io.MultiWriter(&sb, io.Discard), "", 0))
	s := sb.String()
	for _, want := range []string{"POST /users/staff", "block_end_date", "GET /planner", "end_date", "is_block = 0", "block_date = \"2026-10-12\""} {
		if !strings.Contains(s, want) {
			t.Fatalf("report lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "/devices/state") {
		t.Fatalf("unrelated methods must not be listed:\n%s", s)
	}
	if strings.Contains(s, "Раббимова") || strings.Contains(s, "BASE64DATA") {
		t.Fatalf("name or photo leaked into the report:\n%s", s)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "perco-api-docs.json")); err != nil || !strings.Contains(string(b), "/planner") {
		t.Fatalf("spec file not saved: %v", err)
	}
}

func TestDiscoverWithoutSpecStillInspectsCard(t *testing.T) {
	var sb strings.Builder
	discover(context.Background(), fakeRaw{"/api/users/staff/960": `{"is_block":1}`}, 960, t.TempDir(), log.New(&sb, "", 0))
	if !strings.Contains(sb.String(), "описание API не найдено") || !strings.Contains(sb.String(), "is_block = 1") {
		t.Fatalf("report:\n%s", sb.String())
	}
}
